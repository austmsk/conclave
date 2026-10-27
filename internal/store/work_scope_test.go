package store_test

import (
	"errors"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
	"github.com/austmsk/conclave/internal/store"
)

// TestWorkRowsOpenRouterScope is the end-to-end case for the work-table
// functions: rows made by EnsureRepo and the Create functions, and nothing
// else, are enough for llm.Router.Scope's attribution check, and a call
// through the scope passes the router's tier check against the row
// EnsureRepo wrote (FR-SEC12) and is charged to the attempt. The router
// is the real one, over the store's directory and ledger, as the
// application role.
func TestWorkRowsOpenRouterScope(t *testing.T) {
	ctx, tx := testTx(t)
	const name = "austmsk/scope"
	// The row starts out of step with configuration; EnsureRepo puts it
	// right, as the worker does at startup.
	if _, err := tx.Exec(ctx, `INSERT INTO repos (full_name, tier, default_branch) VALUES ($1, 'sensitive', 'main')`, name); err != nil {
		t.Fatal(err)
	}
	c := createChain(t, ctx, tx, name, 7)
	other := createChain(t, ctx, tx, "austmsk/scope-other", 8)

	provider := faketest.New(faketest.Turn{Response: faketest.Response{
		Text: "done", StopReason: contracts.StopEndTurn, InputTokens: 40, OutputTokens: 10,
	}})
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": {
			Provider: llm.ProviderAnthropic, InputUSDPerMTok: 3, OutputUSDPerMTok: 15, MaxOutputTokens: 4096,
		}},
		Roles:     map[contracts.Role]llm.RoleRoute{contracts.RoleImplementer: {Model: "m"}},
		RateLimit: llm.RateLimit{RequestsPerMinute: 1000, TokensPerMinute: 1_000_000},
		Budgets:   llm.Budgets{FeatureRequestTokens: 1_000_000, MonthlyUSD: 100},
	}
	repos := llm.NewDirectoryRepos(store.NewRepoDirectory(tx), map[string]llm.RepoAccess{
		name: {Tier: contracts.TierPublic, Roles: []contracts.Role{contracts.RoleImplementer}},
	})
	r, err := llm.New(cfg, map[string]llm.Provider{llm.ProviderAnthropic: provider}, repos, store.NewCostLedger(tx))
	if err != nil {
		t.Fatal(err)
	}

	scope := llm.Scope{RepoID: c.repo.ID, FeatureRequestID: c.feature.ID, AttemptID: c.attempt.ID,
		KeyPrefix: "feature-7/attempt-1", TokenBudget: 10_000}
	scoped, err := r.Scope(ctx, scope)
	if err != nil {
		t.Fatalf("Scope over rows from the store functions: %v", err)
	}
	if _, err := scoped.Complete(ctx, contracts.Request{
		RepoID: c.repo.ID, PromptVersion: "implementer-v1", Role: contracts.RoleImplementer, MaxTokens: 100,
		Messages: []contracts.Message{{Role: contracts.MessageUser, Text: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM cost_events WHERE attempt_id = $1 AND feature_request_id = $2`,
		c.attempt.ID, c.feature.ID); n != 1 {
		t.Errorf("%d cost events charged to the attempt, want 1", n)
	}

	feature := scope
	feature.AttemptID, feature.TokenBudget = "", 0
	if _, err := r.FeatureRequestScope(ctx, feature); err != nil {
		t.Errorf("FeatureRequestScope: %v", err)
	}
	// The attribution check is real: another feature request's attempt
	// is refused.
	crossed := scope
	crossed.AttemptID = other.attempt.ID
	if _, err := r.Scope(ctx, crossed); !errors.Is(err, llm.ErrInvalidScope) {
		t.Errorf("Scope with another feature request's attempt: got %v, want ErrInvalidScope", err)
	}
}
