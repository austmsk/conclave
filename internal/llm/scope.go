package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// Scope bounds a run of model calls and attributes their spend. RepoID,
// FeatureRequestID, and AttemptID are database IDs, which are UUIDs, and
// the constructors refuse anything else. Checking there, before any call,
// is what matters: an ID the ledger refused only after a billed reply would
// fail the call with spend that was never recorded.
//
// The budgets are not held in the scope. Before each model call the ledger
// sums what is settled and reserved for the attempt, the feature request,
// and the month, across every scope and process, and reserves the call's
// claim. Two live scopes for one attempt, such as an activity abandoned
// after a heartbeat timeout and its retry, therefore share one budget, and
// a call still in flight in either counts against both.
type Scope struct {
	// RepoID is the repository whose workspace the scope runs against.
	// Every request must name the same repository, so a request built from
	// the wrong input cannot be checked against another repository's tier.
	RepoID string

	// FeatureRequestID is charged for every call, and its token cap
	// applies. Required.
	FeatureRequestID string

	// AttemptID is charged for every call, and TokenBudget is its cap.
	// Required by Router.Scope; Router.FeatureRequestScope refuses it.
	AttemptID string

	// KeyPrefix namespaces this scope's cost events, for example the
	// workflow and activity IDs.
	KeyPrefix string

	// TokenBudget caps combined input and output tokens for the attempt,
	// over every scope opened for it (FR-SEC9).
	TokenBudget int
}

// Scoped is a contracts.Router bound to one Scope. It is safe for
// concurrent use.
type Scoped struct {
	r     *Router
	scope Scope

	mu   sync.Mutex
	used int
}

var _ contracts.Router = (*Scoped)(nil)

// settleTimeout bounds recording a billed reply once the caller's context
// is gone. The reply is paid for, so it is recorded even then.
const settleTimeout = 30 * time.Second

// Scope returns a router bound to one attempt. The repository, feature
// request, attempt, key prefix, and a positive budget are all required, and
// the attempt must belong to the feature request and the feature request to
// the repository: without that, spend could be charged where no cap sees
// it.
func (r *Router) Scope(ctx context.Context, s Scope) (*Scoped, error) {
	switch {
	case s.AttemptID == "":
		return nil, fmt.Errorf("%w: no attempt: use FeatureRequestScope for calls outside an attempt", ErrInvalidScope)
	case s.TokenBudget <= 0:
		return nil, fmt.Errorf("%w: budget %d", ErrInvalidScope, s.TokenBudget)
	}
	return r.newScoped(ctx, s)
}

// FeatureRequestScope returns a router for a feature request's calls made
// outside any attempt, such as analysis. It has no budget of its own; the
// feature request and monthly caps apply to it as to every call. A scope
// that names an attempt or a budget is refused, so attempt work cannot be
// moved out from under its attempt's budget by using this constructor.
func (r *Router) FeatureRequestScope(ctx context.Context, s Scope) (*Scoped, error) {
	switch {
	case s.AttemptID != "":
		return nil, fmt.Errorf("%w: attempt %s: use Scope for calls in an attempt", ErrInvalidScope, s.AttemptID)
	case s.TokenBudget != 0:
		return nil, fmt.Errorf("%w: a feature request scope has no budget of its own, got %d", ErrInvalidScope, s.TokenBudget)
	}
	return r.newScoped(ctx, s)
}

// newScoped checks what every scope needs and that its IDs are attributed.
func (r *Router) newScoped(ctx context.Context, s Scope) (*Scoped, error) {
	switch {
	case s.RepoID == "":
		return nil, fmt.Errorf("%w: no repository", ErrInvalidScope)
	case s.FeatureRequestID == "":
		return nil, fmt.Errorf("%w: no feature request", ErrInvalidScope)
	case s.KeyPrefix == "":
		return nil, fmt.Errorf("%w: empty key prefix", ErrInvalidScope)
	}
	if err := s.checkIDs(); err != nil {
		return nil, err
	}
	ok, err := r.costs.Attributed(ctx, s.RepoID, s.FeatureRequestID, s.AttemptID)
	if err != nil {
		return nil, fmt.Errorf("checking attribution of feature request %s: %w", s.FeatureRequestID, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: attempt %q is not in feature request %s of repository %s",
			ErrInvalidScope, s.AttemptID, s.FeatureRequestID, s.RepoID)
	}
	return &Scoped{r: r, scope: s}, nil
}

// checkIDs requires every ID the scope carries to be a UUID. The attempt
// may be absent.
func (s Scope) checkIDs() error {
	ids := []struct{ field, v string }{
		{"repository", s.RepoID}, {"feature request", s.FeatureRequestID}, {"attempt", s.AttemptID},
	}
	for _, id := range ids {
		if id.v != "" && !isUUID(id.v) {
			return fmt.Errorf("%w: %s ID %q is not a UUID", ErrInvalidScope, id.field, id.v)
		}
	}
	return nil
}

// isUUID accepts the canonical 8-4-4-4-12 hex form, the form Postgres
// writes IDs in. It is stricter than Postgres's own parser, so anything it
// accepts Postgres accepts too.
func isUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i := range len(v) {
		c := v[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !isHexDigit(c):
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// checkRepo refuses a request for any repository but the scope's.
func (s *Scoped) checkRepo(repoID string) error {
	if repoID != s.scope.RepoID {
		return fmt.Errorf("request for repository %q in a scope for %q: %w", repoID, s.scope.RepoID, ErrRepoMismatch)
	}
	return nil
}

// TokensUsed reports the tokens settled by calls made through this scope.
// Spend by other scopes for the same attempt is not included, though it
// counts against the budget.
func (s *Scoped) TokensUsed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// reserve claims input tokens and up to wantOutput output tokens on model m
// from every cap that applies, and returns the reservation and the output
// granted. A call can still overshoot: input is an estimate. The next call
// is then refused.
func (s *Scoped) reserve(ctx context.Context, m ModelConfig, input, wantOutput int) (string, int, error) {
	a := ask{model: m, input: input, wantOutput: wantOutput, attemptBudget: s.scope.TokenBudget, budgets: s.r.cfg.Budgets}
	var output int
	id, err := s.r.costs.Reserve(ctx, s.scope.AttemptID, s.scope.FeatureRequestID, reservationTTL,
		func(sp Spend) (Claim, error) {
			out, c, err := a.decide(sp)
			output = out
			return c, err
		})
	if err != nil {
		return "", 0, err
	}
	return id, output, nil
}

// settle records one reply's cost event and releases its reservation, or
// only releases it when the call billed nothing. It runs even when ctx is
// cancelled: the provider has been paid, and spend that is not recorded
// cannot be capped. A failure fails the call, and the reservation stays
// until it expires, so the spend still counts meanwhile. OccurredAt is left
// for the ledger to stamp, so the router reads no clock and stays usable
// from code that must be deterministic.
func (s *Scoped) settle(ctx context.Context, reservationID string, role contracts.Role, modelID, provider, replyID string, usage contracts.Usage) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	tokens := usage.InputTokens + usage.OutputTokens
	if tokens == 0 && usage.CostUSD == 0 {
		if err := s.r.costs.Release(ctx, reservationID); err != nil {
			return fmt.Errorf("releasing reservation for %s call to %s: %w", role, modelID, err)
		}
		return nil
	}
	ev := contracts.CostEvent{
		IdempotencyKey:   costKey(s.scope.KeyPrefix, provider, replyID),
		RepoID:           s.scope.RepoID,
		FeatureRequestID: s.scope.FeatureRequestID,
		AttemptID:        s.scope.AttemptID,
		Role:             role,
		Provider:         provider,
		Model:            modelID,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CostUSD:          usage.CostUSD,
	}
	if err := s.r.costs.Settle(ctx, reservationID, ev); err != nil {
		return fmt.Errorf("charging %s call to %s: %w", role, modelID, err)
	}
	s.mu.Lock()
	s.used += tokens
	s.mu.Unlock()
	return nil
}
