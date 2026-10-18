package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"

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
	claim := reservation{input: estimateInputTokens(req), output: s.r.maxOutput(route)}
	if req.MaxTokens > 0 {
		claim.output = min(claim.output, req.MaxTokens)
	}

	res, err := s.callChain(ctx, req, policy, route, claim)
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

// reservation is what a request asks of the budgets before any model is
// chosen: the estimated input and the most output it wants. Each call in
// the chain reserves it again, capped at that model's output limit, and
// may be granted less output.
type reservation struct{ input, output int }

// chainResult is the model and reply that ended a chain.
type chainResult struct {
	modelID string
	resp    ProviderResponse
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

// call sends one request under the rate limit and the budgets, and records
// whatever the provider billed for it, whether or not the call succeeded.
//
// The rate limiter is waited on first, so the reservation is taken just
// before the provider is reached and its expiry covers the call rather than
// the queue. A refused reservation hands its rate-limit claim back. The
// usage is normalized by billable as soon as the provider returns, and the
// response call returns carries that normalized usage, so the rate limiter,
// the cost event, and the caller all charge the same number. A recording
// failure replaces the call's own error, so it is never mistaken for
// unavailability and passed over for a fallback.
func (s *Scoped) call(ctx context.Context, req contracts.Request, modelID string, claim reservation) (ProviderResponse, error) {
	model := s.r.cfg.Models[modelID]
	want := min(claim.output, model.MaxOutputTokens)
	limited := claim.input + want
	if err := s.r.limit.acquire(ctx, limited); err != nil {
		return ProviderResponse{}, err
	}
	held, output, err := s.reserve(ctx, model, claim.input, want)
	if err != nil {
		s.r.limit.settle(limited, 0)
		return ProviderResponse{}, err
	}
	resp, err := s.r.providers[model.Provider].Complete(ctx, ProviderRequest{
		Model:     modelID,
		System:    req.System,
		Messages:  req.Messages,
		Tools:     req.Tools,
		MaxTokens: output,
	})
	resp = billable(resp, err == nil, claim.input, output)
	s.r.limit.settle(limited, resp.totalTokens())
	if rerr := s.settle(ctx, held, req.Role, modelID, model.Provider, resp.ID, usageOf(model, resp)); rerr != nil {
		if err != nil {
			return resp, fmt.Errorf("%w (after the call failed: %v)", rerr, err)
		}
		return resp, rerr
	}
	return resp, err
}
