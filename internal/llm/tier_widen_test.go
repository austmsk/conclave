package llm_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// TestConfigCannotWidenTier attempts to widen a non-sensitive tier through
// the owner's access file: a private or local-only repository whose entry
// approves every provider in sensitive_providers. That list is read only for
// the sensitive tier, so each request must get exactly what the hardcoded
// table allows, and a provider the tier disallows must never see a request
// (security-and-threat-model.md, Verification).
func TestConfigCannotWidenTier(t *testing.T) {
	for _, tier := range []contracts.SensitivityTier{contracts.TierPrivate, contracts.TierLocalOnly} {
		for _, provider := range allProviders {
			t.Run(fmt.Sprintf("%s/%s", tier, provider), func(t *testing.T) {
				widenCase(t, tier, provider)
			})
		}
	}
}

func widenCase(t *testing.T, tier contracts.SensitivityTier, provider string) {
	const name = "austmsk/widened"
	src := fmt.Sprintf("repositories:\n  %s:\n    tier: %s\n    roles: [planner]\n    sensitive_providers: [%s]\n",
		name, tier, strings.Join(allProviders, ", "))
	access, err := llm.LoadRepoAccess(strings.NewReader(src))
	if err != nil {
		t.Fatalf("LoadRepoAccess: %v", err)
	}
	dir := directory{repoID: {ID: repoID, FullName: name, Tier: tier}}

	emb := &stubEmbedder{Provider: faketest.New(endTurn(10, 5))}
	p := providers(fakes())
	p[provider] = emb
	cfg := llm.Config{
		Budgets:   testBudgets,
		Models:    map[string]llm.ModelConfig{"m": model(provider)},
		Roles:     map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
		Embedding: &llm.EmbeddingRoute{Model: "m"},
	}
	r, err := llm.New(cfg, p, llm.NewDirectoryRepos(dir, access), newCosts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 100000})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}

	allowed := tierAllows[tier][provider]
	_, err = s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
	assertTierOutcome(t, allowed, err)
	_, err = s.Embed(context.Background(), repoID, []string{"x"})
	assertTierOutcome(t, allowed, err)

	want := 0
	if allowed {
		want = 1
	}
	if n := len(emb.Requests()); n != want {
		t.Fatalf("provider received %d completion requests, want %d", n, want)
	}
	if emb.calls != want {
		t.Fatalf("provider received %d embedding requests, want %d", emb.calls, want)
	}
}
