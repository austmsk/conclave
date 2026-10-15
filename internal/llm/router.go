package llm

import (
	"context"
	"errors"
	"fmt"
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

// Router holds the validated configuration, the provider adapters, and the
// repository policies. It does not serve requests itself: callers obtain a
// Scoped router, which carries the budget and the idempotency key prefix
// every request needs.
type Router struct {
	cfg       Config
	providers map[string]Provider
	repos     Repos
	costs     CostRecorder
}

// New validates cfg and checks that every provider it routes to has an
// adapter, so a missing adapter fails at startup rather than mid-run.
func New(cfg Config, providers map[string]Provider, repos Repos, costs CostRecorder) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if repos == nil || costs == nil {
		return nil, errors.New("router needs repository policies and a cost recorder")
	}
	for _, id := range sortedKeys(cfg.Models) {
		name := cfg.Models[id].Provider
		if providers[name] == nil {
			return nil, fmt.Errorf("model %q routes to provider %q: %w", id, name, ErrMissingProvider)
		}
	}
	return &Router{cfg: cfg, providers: providers, repos: repos, costs: costs}, nil
}

// Scope bounds a run of model calls, normally one attempt.
type Scope struct {
	FeatureRequestID string
	AttemptID        string

	// KeyPrefix makes cost events idempotent. It must be stable across
	// retries of the same activity, for example derived from the workflow
	// and activity IDs, so a retry replays the same keys.
	KeyPrefix string

	// TokenBudget caps combined input and output tokens for the scope
	// (FR-SEC9). TokensUsed seeds the count with spend already recorded,
	// so a retried activity does not get a fresh budget.
	TokenBudget int
	TokensUsed  int
}

// Scoped is a contracts.Router bound to one Scope. It is safe for
// concurrent use.
type Scoped struct {
	r     *Router
	scope Scope

	mu   sync.Mutex
	used int
	seq  int
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

// TokensUsed reports the scope's spend so far.
func (s *Scoped) TokensUsed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// Complete routes req to the role's model, checking the repository's tier
// for every model in the chain as it is reached. A fallback is tried only
// when the previous model was unavailable, and a fallback the tier
// disallows ends the request with ErrTierDenied: it is never skipped in
// favour of something further down the chain (RL-17).
func (s *Scoped) Complete(ctx context.Context, req contracts.Request) (contracts.Response, error) {
	if err := validateRequest(req); err != nil {
		return contracts.Response{}, err
	}
	policy, err := lookupPolicy(ctx, s.r.repos, req.RepoID)
	if err != nil {
		return contracts.Response{}, err
	}
	if !policy.allowsRole(req.Role) {
		return contracts.Response{}, fmt.Errorf("role %q on repository %q: %w", req.Role, req.RepoID, ErrRoleDenied)
	}
	route, ok := s.r.cfg.Roles[req.Role]
	if !ok {
		return contracts.Response{}, fmt.Errorf("role %q: %w", req.Role, ErrNoRoute)
	}
	remaining, err := s.remaining()
	if err != nil {
		return contracts.Response{}, err
	}

	var lastErr error
	for _, modelID := range route.chain() {
		model := s.r.cfg.Models[modelID]
		if !policy.allowsProvider(model.Provider) {
			return contracts.Response{}, fmt.Errorf("model %q on provider %q for %s repository %q: %w",
				modelID, model.Provider, policy.Tier, req.RepoID, ErrTierDenied)
		}
		resp, err := s.r.providers[model.Provider].Complete(ctx, providerRequest(req, modelID, model, remaining))
		if errors.Is(err, ErrUnavailable) {
			lastErr = fmt.Errorf("model %q: %w", modelID, err)
			continue
		}
		if err != nil {
			return contracts.Response{}, fmt.Errorf("model %q: %w", modelID, err)
		}
		return s.account(ctx, req, modelID, model, resp)
	}
	return contracts.Response{}, fmt.Errorf("every model for role %q was unavailable: %w", req.Role, lastErr)
}

func validateRequest(req contracts.Request) error {
	switch {
	case req.Role == "":
		return fmt.Errorf("%w: no role", ErrInvalidRequest)
	case len(req.Messages) == 0:
		return fmt.Errorf("%w: no messages", ErrInvalidRequest)
	case req.MaxTokens < 0:
		return fmt.Errorf("%w: negative max tokens", ErrInvalidRequest)
	}
	return nil
}

// remaining returns the tokens left in the budget, refusing the call when
// none are.
func (s *Scoped) remaining() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := s.scope.TokenBudget - s.used
	if left <= 0 {
		return 0, fmt.Errorf("%d of %d tokens used: %w", s.used, s.scope.TokenBudget, ErrBudgetExceeded)
	}
	return left, nil
}

// providerRequest clamps the output limit to the smallest of what the caller
// asked for, the model's maximum, and the remaining budget. Input tokens are
// not known until the provider replies, so a single call can overshoot the
// budget by its input; the next call is then refused.
func providerRequest(req contracts.Request, modelID string, m ModelConfig, remaining int) ProviderRequest {
	limit := min(m.MaxOutputTokens, remaining)
	if req.MaxTokens > 0 {
		limit = min(limit, req.MaxTokens)
	}
	return ProviderRequest{
		Model:     modelID,
		System:    req.System,
		Messages:  req.Messages,
		Tools:     req.Tools,
		MaxTokens: limit,
	}
}

// account charges the call to the budget and records its cost. A failure to
// record fails the call: spend that is not recorded cannot be capped.
func (s *Scoped) account(ctx context.Context, req contracts.Request, modelID string, m ModelConfig, resp ProviderResponse) (contracts.Response, error) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.used += resp.InputTokens + resp.OutputTokens
	s.mu.Unlock()

	usage := contracts.Usage{
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
		CostUSD:      costUSD(m, resp.InputTokens, resp.OutputTokens),
	}
	ev := contracts.CostEvent{
		IdempotencyKey:   fmt.Sprintf("%s/%d", s.scope.KeyPrefix, seq),
		RepoID:           req.RepoID,
		FeatureRequestID: s.scope.FeatureRequestID,
		AttemptID:        s.scope.AttemptID,
		Role:             req.Role,
		Provider:         m.Provider,
		Model:            modelID,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CostUSD:          usage.CostUSD,
		OccurredAt:       time.Now().UTC(),
	}
	if err := s.r.costs.RecordCost(ctx, ev); err != nil {
		return contracts.Response{}, fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
	}
	return contracts.Response{
		Provenance: contracts.Provenance{Role: req.Role, Model: modelID, Provider: m.Provider},
		Usage:      usage,
		Text:       resp.Text,
		ToolCalls:  resp.ToolCalls,
		StopReason: resp.StopReason,
	}, nil
}

// Embed produces vectors with the configured embedding model, under the same
// tier check as Complete. Embedding calls are not role-scoped and are not
// charged to the token budget.
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
		return nil, fmt.Errorf("provider %q does not serve embeddings", model.Provider)
	}
	vecs, err := emb.Embed(ctx, modelID, texts)
	if err != nil {
		return nil, fmt.Errorf("embedding with %q: %w", modelID, err)
	}
	return vecs, nil
}
