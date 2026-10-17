package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/austmsk/conclave/internal/contracts"
)

// Router errors that are not policy decisions.
var (
	ErrNoRoute         = errors.New("no model is configured for this role")
	ErrBudgetExceeded  = errors.New("token budget exhausted")
	ErrInvalidRequest  = errors.New("invalid model request")
	ErrMissingProvider = errors.New("no adapter registered for provider")
	ErrInvalidScope    = errors.New("invalid router scope")
	ErrRepoMismatch    = errors.New("request names a different repository than its scope")
)

// embeddingCostRole labels embedding spend in cost events. It is not an
// agent role and appears in no allowlist.
const embeddingCostRole contracts.Role = "embedding"

// Router holds the validated configuration, the provider adapters, the
// repository policies, and the rate limiter every scope shares. It does not
// serve requests itself: callers obtain a Scoped router, which carries the
// budget and the idempotency key prefix every request needs.
type Router struct {
	cfg       Config
	providers map[string]Provider
	repos     Repos
	costs     CostLedger
	limit     *limiter
}

// New validates cfg and checks that every provider it routes to has an
// adapter, so a missing adapter fails at startup rather than mid-run, and
// validates repos for the same reason. It keeps copies of cfg and providers, so later changes by the caller cannot
// undo the validation.
func New(cfg Config, providers map[string]Provider, repos Repos, costs CostLedger) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if repos == nil || costs == nil {
		return nil, errors.New("router needs repository policies and a cost ledger")
	}
	if err := repos.Validate(); err != nil {
		return nil, fmt.Errorf("validating repository policies: %w", err)
	}
	cfg = cfg.clone()
	providers = maps.Clone(providers)
	for _, id := range sortedKeys(cfg.Models) {
		name := cfg.Models[id].Provider
		if providers[name] == nil {
			return nil, fmt.Errorf("model %q routes to provider %q: %w", id, name, ErrMissingProvider)
		}
	}
	r := &Router{cfg: cfg, providers: providers, repos: repos, costs: costs}
	r.limit = newLimiter(cfg.RateLimit.withDefaults())
	return r, nil
}

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

// Complete routes req to the role's model. The repository's tier is checked
// for every model in the chain as it is reached; a fallback is tried only
// when the previous model was unavailable, and a fallback the tier
// disallows ends the request with ErrTierDenied. It is never skipped in
// favour of something further down the chain (RL-17).
//
// Every reply the provider bills is charged and recorded, including one the
// adapter then rejects, so a model that keeps refusing still exhausts the
// budget. A successful reply that reports no usage is charged what was
// reserved for it, so a model that reports nothing exhausts it too.
func (s *Scoped) Complete(ctx context.Context, req contracts.Request) (contracts.Response, error) {
	if err := s.checkRepo(req.RepoID); err != nil {
		return contracts.Response{}, err
	}
	policy, route, err := s.r.authorize(ctx, req)
	if err != nil {
		return contracts.Response{}, err
	}
	estimate := estimateInputTokens(req)
	wantOutput := s.r.maxOutput(route)
	if req.MaxTokens > 0 {
		wantOutput = min(wantOutput, req.MaxTokens)
	}
	output, err := s.reserve(estimate, wantOutput)
	if err != nil {
		return contracts.Response{}, err
	}
	claim := reservation{input: estimate, output: output}

	res, err := s.callChain(ctx, req, policy, route, claim)
	s.settle(claim.total(), res.spent)
	if err != nil {
		return contracts.Response{}, err
	}
	model := s.r.cfg.Models[res.modelID]
	resp := res.resp
	return contracts.Response{
		Provenance: contracts.Provenance{
			Role:          req.Role,
			PromptVersion: req.PromptVersion,
			Model:         res.modelID,
			Provider:      model.Provider,
		},
		Usage:      usageOf(model, resp),
		Text:       resp.Text,
		ToolCalls:  resp.ToolCalls,
		StopReason: resp.StopReason,
	}, nil
}

// authorize validates the request and checks the repository's policy and
// the role's route, all before any provider is involved.
func (r *Router) authorize(ctx context.Context, req contracts.Request) (RepoPolicy, RoleRoute, error) {
	if err := validateRequest(req); err != nil {
		return RepoPolicy{}, RoleRoute{}, err
	}
	policy, err := lookupPolicy(ctx, r.repos, req.RepoID)
	if err != nil {
		return RepoPolicy{}, RoleRoute{}, err
	}
	if !policy.allowsRole(req.Role) {
		return RepoPolicy{}, RoleRoute{}, fmt.Errorf("role %q on repository %q: %w", req.Role, req.RepoID, ErrRoleDenied)
	}
	route, ok := r.cfg.Roles[req.Role]
	if !ok {
		return RepoPolicy{}, RoleRoute{}, fmt.Errorf("role %q: %w", req.Role, ErrNoRoute)
	}
	return policy, route, nil
}

func validateRequest(req contracts.Request) error {
	switch {
	case req.Role == "":
		return fmt.Errorf("%w: no role", ErrInvalidRequest)
	case req.PromptVersion == "":
		// Without it, outputs cannot be compared across prompt changes.
		return fmt.Errorf("%w: no prompt version", ErrInvalidRequest)
	case len(req.Messages) == 0:
		return fmt.Errorf("%w: no messages", ErrInvalidRequest)
	case req.MaxTokens < 0:
		return fmt.Errorf("%w: negative max tokens", ErrInvalidRequest)
	}
	return nil
}

// maxOutput is the largest output limit of any model in the chain.
func (r *Router) maxOutput(route RoleRoute) int {
	most := 0
	for _, id := range route.chain() {
		most = max(most, r.cfg.Models[id].MaxOutputTokens)
	}
	return most
}

// reservation is what Complete claimed from the budget for one request: the
// estimated input and the output tokens granted.
type reservation struct{ input, output int }

func (r reservation) total() int { return r.input + r.output }

// chainResult is the reply that ended a chain and the tokens billed by
// every model the chain tried, which may be more than the reply's own.
type chainResult struct {
	modelID string
	resp    ProviderResponse
	spent   int
}

// callChain tries each model in turn until one replies, the tier denies
// one, or an error other than unavailability occurs.
func (s *Scoped) callChain(ctx context.Context, req contracts.Request, policy RepoPolicy, route RoleRoute, claim reservation) (chainResult, error) {
	var res chainResult
	var lastErr error
	for _, modelID := range route.chain() {
		model := s.r.cfg.Models[modelID]
		if !policy.allowsProvider(model.Provider) {
			return res, fmt.Errorf("model %q on provider %q for %s repository %q: %w",
				modelID, model.Provider, policy.Tier, req.RepoID, ErrTierDenied)
		}
		resp, err := s.call(ctx, req, modelID, claim)
		res.spent += resp.totalTokens()
		if errors.Is(err, ErrUnavailable) {
			lastErr = fmt.Errorf("model %q: %w", modelID, err)
			continue
		}
		if err != nil {
			return res, fmt.Errorf("model %q: %w", modelID, err)
		}
		res.modelID, res.resp = modelID, resp
		return res, nil
	}
	return res, fmt.Errorf("every model for role %q was unavailable: %w", req.Role, lastErr)
}

// call sends one request under the rate limit and records whatever the
// provider billed for it, whether or not the call succeeded. The usage is
// normalized by billable as soon as the provider returns, and the response
// call returns carries that normalized usage, so the rate limiter, the cost
// event, and the caller's settle all charge the same number. A recording
// failure replaces the call's own error, so it is never mistaken for
// unavailability and passed over for a fallback.
func (s *Scoped) call(ctx context.Context, req contracts.Request, modelID string, claim reservation) (ProviderResponse, error) {
	if err := s.r.limit.acquire(ctx, claim.total()); err != nil {
		return ProviderResponse{}, err
	}
	model := s.r.cfg.Models[modelID]
	maxTokens := min(claim.output, model.MaxOutputTokens)
	resp, err := s.r.providers[model.Provider].Complete(ctx, ProviderRequest{
		Model:     modelID,
		System:    req.System,
		Messages:  req.Messages,
		Tools:     req.Tools,
		MaxTokens: maxTokens,
	})
	resp = billable(resp, err == nil, claim.input, maxTokens)
	s.r.limit.settle(claim.total(), resp.totalTokens())
	if err != nil && resp.totalTokens() == 0 {
		return resp, err
	}
	if rerr := s.record(ctx, req.Role, modelID, model.Provider, resp.ID, usageOf(model, resp)); rerr != nil {
		if err != nil {
			return resp, fmt.Errorf("%w (after the call failed: %v)", rerr, err)
		}
		return resp, rerr
	}
	return resp, err
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

// Embed produces vectors with the configured embedding model, under the same
// tier check, budget, rate limit, and cost recording as Complete. Embedding
// calls are not role-scoped.
func (s *Scoped) Embed(ctx context.Context, repoID string, texts []string) ([][]float32, error) {
	if err := s.checkRepo(repoID); err != nil {
		return nil, err
	}
	policy, err := lookupPolicy(ctx, s.r.repos, repoID)
	if err != nil {
		return nil, err
	}
	if s.r.cfg.Embedding == nil {
		return nil, ErrNoEmbeddingModel
	}
	modelID := s.r.cfg.Embedding.Model
	model := s.r.cfg.Models[modelID]
	if !policy.allowsProvider(model.Provider) {
		return nil, fmt.Errorf("embedding model %q on provider %q for %s repository %q: %w",
			modelID, model.Provider, policy.Tier, repoID, ErrTierDenied)
	}
	emb, ok := s.r.providers[model.Provider].(Embedder)
	if !ok {
		return nil, fmt.Errorf("provider %q: %w", model.Provider, ErrNoEmbeddingModel)
	}
	resp, err := s.embed(ctx, emb, modelID, texts)
	if err != nil {
		return nil, err
	}
	return resp.Vectors, nil
}

// embed reserves, rate-limits, calls, settles, and records one embedding
// request. Its usage is normalized by billable, as a completion's is: a
// negative count is ignored and a provider that reports no usage on success
// is charged the estimate. A failed call is recorded only if the provider
// reported usage.
// Embedding replies need not carry an ID, so their cost events get random
// keys and a retried write is counted twice. Milestone 2, which brings
// embeddings into use, should require an ID or derive the key from the
// request.
func (s *Scoped) embed(ctx context.Context, emb Embedder, modelID string, texts []string) (EmbedResponse, error) {
	estimate := estimateTextTokens(texts...)
	if _, err := s.reserve(estimate, 0); err != nil {
		return EmbedResponse{}, err
	}
	if err := s.r.limit.acquire(ctx, estimate); err != nil {
		s.settle(estimate, 0)
		return EmbedResponse{}, err
	}
	resp, err := emb.Embed(ctx, modelID, texts)
	used := billable(ProviderResponse{InputTokens: resp.InputTokens}, err == nil, estimate, 0).InputTokens
	s.r.limit.settle(estimate, used)
	s.settle(estimate, used)
	if err != nil && used == 0 {
		return EmbedResponse{}, fmt.Errorf("embedding with %q: %w", modelID, err)
	}
	model := s.r.cfg.Models[modelID]
	usage := contracts.Usage{InputTokens: used, CostUSD: costUSD(model, ProviderResponse{InputTokens: used})}
	if rerr := s.record(ctx, embeddingCostRole, modelID, model.Provider, resp.ID, usage); rerr != nil {
		return EmbedResponse{}, rerr
	}
	if err != nil {
		return EmbedResponse{}, fmt.Errorf("embedding with %q: %w", modelID, err)
	}
	return resp, nil
}
