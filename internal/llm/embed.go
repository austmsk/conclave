package llm

import (
	"context"
	"fmt"

	"github.com/austmsk/conclave/internal/contracts"
)

// embeddingCostRole labels embedding spend in cost events. It is not an
// agent role and appears in no allowlist.
const embeddingCostRole contracts.Role = "embedding"

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
