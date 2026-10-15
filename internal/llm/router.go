package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// Router errors that are not policy decisions.
var (
	ErrNoRoute         = errors.New("no model is configured for this role")
	ErrBudgetExceeded  = errors.New("token budget exhausted")
	ErrInvalidRequest  = errors.New("invalid model request")
	ErrMissingProvider = errors.New("no adapter registered for provider")
	ErrInvalidScope    = errors.New("invalid router scope")
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
	costs     CostRecorder
	limit     *limiter
}

// New validates cfg and checks that every provider it routes to has an
// adapter, so a missing adapter fails at startup rather than mid-run. It
// keeps copies of cfg and providers, so later changes by the caller cannot
// undo the validation.
func New(cfg Config, providers map[string]Provider, repos Repos, costs CostRecorder) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if repos == nil || costs == nil {
		return nil, errors.New("router needs repository policies and a cost recorder")
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

// Scope bounds a run of model calls, normally one attempt.
type Scope struct {
	FeatureRequestID string
	AttemptID        string

	// KeyPrefix namespaces this scope's cost events, for example the
	// workflow and activity IDs.
	KeyPrefix string

	// TokenBudget caps combined input and output tokens for the scope
	// (FR-SEC9). TokensUsed seeds the count with spend already recorded,
	// so a retried activity does not get a fresh budget.
	TokenBudget int
	TokensUsed  int
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

// Scope returns a router bound to s. A scope without a key prefix or a
// positive budget is refused: neither cost recording nor budget enforcement
// may be optional.
func (r *Router) Scope(s Scope) (*Scoped, error) {
	if s.KeyPrefix == "" {
		return nil, fmt.Errorf("%w: empty key prefix", ErrInvalidScope)
	}
	if s.TokenBudget <= 0 || s.TokensUsed < 0 {
		return nil, fmt.Errorf("%w: budget %d with %d used", ErrInvalidScope, s.TokenBudget, s.TokensUsed)
	}
	return &Scoped{r: r, scope: s, used: s.TokensUsed}, nil
}

// TokensUsed reports the scope's settled spend so far.
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
func (s *Scoped) Complete(ctx context.Context, req contracts.Request) (contracts.Response, error) {
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
	reserved := estimate + output

	modelID, resp, err := s.callChain(ctx, req, policy, route, output, reserved)
	if err != nil {
		s.settle(reserved, 0)
		return contracts.Response{}, err
	}
	s.settle(reserved, resp.totalTokens())

	model := s.r.cfg.Models[modelID]
	usage := contracts.Usage{
		InputTokens:  resp.InputTokens + resp.CacheWriteTokens + resp.CacheReadTokens,
		OutputTokens: resp.OutputTokens,
		CostUSD:      costUSD(model, resp),
	}
	if err := s.record(ctx, req.RepoID, req.Role, modelID, model.Provider, resp.ID, usage); err != nil {
		return contracts.Response{}, err
	}
	return contracts.Response{
		Provenance: contracts.Provenance{
			Role:          req.Role,
			PromptVersion: req.PromptVersion,
			Model:         modelID,
			Provider:      model.Provider,
		},
		Usage:      usage,
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

// callChain tries each model in turn until one replies, the tier denies
// one, or an error other than unavailability occurs.
func (s *Scoped) callChain(ctx context.Context, req contracts.Request, policy RepoPolicy, route RoleRoute, output, reserved int) (string, ProviderResponse, error) {
	var lastErr error
	for _, modelID := range route.chain() {
		model := s.r.cfg.Models[modelID]
		if !policy.allowsProvider(model.Provider) {
			return "", ProviderResponse{}, fmt.Errorf("model %q on provider %q for %s repository %q: %w",
				modelID, model.Provider, policy.Tier, req.RepoID, ErrTierDenied)
		}
		if err := s.r.limit.acquire(ctx, reserved); err != nil {
			return "", ProviderResponse{}, err
		}
		resp, err := s.r.providers[model.Provider].Complete(ctx, ProviderRequest{
			Model:     modelID,
			System:    req.System,
			Messages:  req.Messages,
			Tools:     req.Tools,
			MaxTokens: min(output, model.MaxOutputTokens),
		})
		s.r.limit.settle(reserved, resp.totalTokens())
		if errors.Is(err, ErrUnavailable) {
			lastErr = fmt.Errorf("model %q: %w", modelID, err)
			continue
		}
		if err != nil {
			return "", ProviderResponse{}, fmt.Errorf("model %q: %w", modelID, err)
		}
		return modelID, resp, nil
	}
	return "", ProviderResponse{}, fmt.Errorf("every model for role %q was unavailable: %w", req.Role, lastErr)
}

// reserve claims input tokens and up to wantOutput output tokens from the
// budget for a call about to be sent, and returns the output granted. The
// call is refused when the estimated input alone does not fit, or when an
// output limit is wanted and no room for output is left. Input is an
// estimate, so a call can still overshoot by the estimate's error; the
// next call is then refused.
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
// spend that is not recorded cannot be capped.
func (s *Scoped) record(ctx context.Context, repoID string, role contracts.Role, modelID, provider, replyID string, usage contracts.Usage) error {
	ev := contracts.CostEvent{
		IdempotencyKey:   costKey(s.scope.KeyPrefix, provider, replyID),
		RepoID:           repoID,
		FeatureRequestID: s.scope.FeatureRequestID,
		AttemptID:        s.scope.AttemptID,
		Role:             role,
		Provider:         provider,
		Model:            modelID,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CostUSD:          usage.CostUSD,
		OccurredAt:       time.Now().UTC(),
	}
	if err := s.r.costs.RecordCost(ctx, ev); err != nil {
		return fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
	}
	return nil
}

// Embed produces vectors with the configured embedding model, under the same
// tier check, budget, rate limit, and cost recording as Complete. Embedding
// calls are not role-scoped.
func (s *Scoped) Embed(ctx context.Context, repoID string, texts []string) ([][]float32, error) {
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
	usage := contracts.Usage{
		InputTokens: resp.InputTokens,
		CostUSD:     costUSD(model, ProviderResponse{InputTokens: resp.InputTokens}),
	}
	if err := s.record(ctx, repoID, embeddingCostRole, modelID, model.Provider, resp.ID, usage); err != nil {
		return nil, err
	}
	return resp.Vectors, nil
}

// embed reserves, rate-limits, calls, and settles one embedding request.
// A provider that reports no usage is charged the estimate.
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
	used := resp.InputTokens
	if err == nil && used == 0 {
		used = estimate
	}
	s.r.limit.settle(estimate, used)
	s.settle(estimate, used)
	if err != nil {
		return EmbedResponse{}, fmt.Errorf("embedding with %q: %w", modelID, err)
	}
	return resp, nil
}
