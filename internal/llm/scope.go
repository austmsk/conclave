package llm

import (
	"context"
	"fmt"
	"sync"

	"github.com/austmsk/conclave/internal/contracts"
)

// Scope bounds a run of model calls, normally one attempt. RepoID,
// FeatureRequestID, and AttemptID are database IDs, which are UUIDs, and
// Router.Scope refuses anything else. Checking there, before any call, is
// what matters: an ID the ledger refused only after a billed reply would
// fail the call with spend that was never recorded.
//
// The budget is seeded from the ledger once, when the scope is created, and
// tracked in memory after that. Two live scopes for the same attempt, such
// as an activity abandoned after a heartbeat timeout and its retry, each
// see only their own calls, so together they can spend up to twice the
// budget. Ticket 8 closes this when activity granularity is settled.
type Scope struct {
	// RepoID is the repository whose workspace the scope runs against.
	// Every request must name the same repository, so a request built from
	// the wrong input cannot be checked against another repository's tier.
	RepoID string

	FeatureRequestID string

	// AttemptID, when set, seeds the budget with the spend the ledger has
	// already recorded for the attempt, so a retried activity does not get
	// a fresh budget. A scope without one starts from zero each time.
	AttemptID string

	// KeyPrefix namespaces this scope's cost events, for example the
	// workflow and activity IDs.
	KeyPrefix string

	// TokenBudget caps combined input and output tokens for the scope
	// (FR-SEC9).
	TokenBudget int
}

// Scoped is a contracts.Router bound to one Scope. It is safe for
// concurrent use: each call reserves its estimated tokens before it is
// sent, so concurrent calls cannot all spend the same headroom.
type Scoped struct {
	r     *Router
	scope Scope

	mu       sync.Mutex
	used     int
	reserved int
}

var _ contracts.Router = (*Scoped)(nil)

// Scope returns a router bound to s. A scope without a repository, a key
// prefix, or a positive budget is refused: neither the tier check, cost
// recording, nor budget enforcement may be optional.
func (r *Router) Scope(ctx context.Context, s Scope) (*Scoped, error) {
	switch {
	case s.RepoID == "":
		return nil, fmt.Errorf("%w: no repository", ErrInvalidScope)
	case s.KeyPrefix == "":
		return nil, fmt.Errorf("%w: empty key prefix", ErrInvalidScope)
	case s.TokenBudget <= 0:
		return nil, fmt.Errorf("%w: budget %d", ErrInvalidScope, s.TokenBudget)
	}
	if err := s.checkIDs(); err != nil {
		return nil, err
	}
	used := 0
	if s.AttemptID != "" {
		n, err := r.costs.AttemptTokens(ctx, s.AttemptID)
		if err != nil {
			return nil, fmt.Errorf("reading recorded spend for attempt %s: %w", s.AttemptID, err)
		}
		used = n
	}
	return &Scoped{r: r, scope: s, used: used}, nil
}

// checkIDs requires every ID the scope carries to be a UUID. The feature
// request and attempt may be absent.
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

// TokensUsed reports the scope's settled spend so far, including what the
// ledger had recorded for the attempt when the scope was created.
func (s *Scoped) TokensUsed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// reserve claims input tokens and up to wantOutput output tokens from the
// budget for a call about to be sent, and returns the output granted. The
// call is refused when the estimated input alone does not fit, or when an
// output limit is wanted and no room for output is left. A call can still
// overshoot: input is an estimate, and the reservation covers one reply
// while a fallback chain is charged for every model that billed, which
// happens only when an unavailable reply still reports usage. The next
// call is then refused.
func (s *Scoped) reserve(input, wantOutput int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := s.scope.TokenBudget - s.used - s.reserved
	output := min(wantOutput, left-input)
	if left <= 0 || input > left || (wantOutput > 0 && output <= 0) {
		return 0, fmt.Errorf("%d used and %d reserved of %d tokens, call needs about %d: %w",
			s.used, s.reserved, s.scope.TokenBudget, input, ErrBudgetExceeded)
	}
	s.reserved += input + output
	return output, nil
}

// settle releases a reservation and charges what the call actually used.
func (s *Scoped) settle(reserved, used int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved -= reserved
	s.used += used
}

// record writes the cost event for one reply. A failure fails the call:
// spend that is not recorded cannot be capped. OccurredAt is left for the
// ledger to stamp, so the router reads no clock and stays usable from code
// that must be deterministic.
func (s *Scoped) record(ctx context.Context, role contracts.Role, modelID, provider, replyID string, usage contracts.Usage) error {
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
	if err := s.r.costs.RecordCost(ctx, ev); err != nil {
		return fmt.Errorf("charging %s call to %s: %w", role, modelID, err)
	}
	return nil
}
