package agenttest

import (
	"context"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// IDs the fake router's scope uses. Scope requires database IDs, which are
// UUIDs.
const (
	RepoID           = "0f6b1c2a-5d1e-4c8e-9a4b-1f2e3d4c5b6a"
	FeatureRequestID = "3c2b1a0f-9e8d-4c7b-a6f5-e4d3c2b1a0f9"
	AttemptID        = "7a1d9e44-2b3c-4f5a-8e6d-0c1b2a3f4e5d"
)

// Model is the one model the fake router routes every role to.
const Model = "fake-model"

// Options adjust the fake router's budgets. Zero fields take defaults high
// enough that only tests about budgets reach them.
type Options struct {
	// TokenBudget is the attempt's FR-SEC9 token cap.
	TokenBudget int
	// Budgets are the platform caps.
	Budgets llm.Budgets
	// MaxOutputTokens is the model's output limit.
	MaxOutputTokens int
}

func (o Options) withDefaults() Options {
	if o.TokenBudget == 0 {
		o.TokenBudget = 1 << 30
	}
	if o.Budgets.FeatureRequestTokens == 0 {
		o.Budgets.FeatureRequestTokens = 1 << 40
	}
	if o.Budgets.MonthlyUSD == 0 {
		o.Budgets.MonthlyUSD = 1e9
	}
	if o.MaxOutputTokens == 0 {
		o.MaxOutputTokens = 4096
	}
	return o
}

// Router returns a scoped router whose provider replays turns, with
// default budgets. Call Remaining on the provider to check every turn was
// used.
func Router(t *testing.T, turns ...faketest.Turn) (*llm.Scoped, *faketest.Provider) {
	t.Helper()
	s, p, _ := RouterWith(t, Options{}, turns...)
	return s, p
}

// RouterWith is Router with explicit budgets. It also returns the cost
// ledger.
func RouterWith(t *testing.T, opts Options, turns ...faketest.Turn) (*llm.Scoped, *faketest.Provider, *faketest.Costs) {
	t.Helper()
	opts = opts.withDefaults()
	provider := faketest.New(turns...)
	costs := &faketest.Costs{Known: []faketest.Attribution{{RepoID: RepoID, FeatureRequestID: FeatureRequestID, AttemptID: AttemptID}}}
	cfg := llm.Config{
		Budgets: opts.Budgets,
		Models: map[string]llm.ModelConfig{Model: {
			Provider: llm.ProviderAnthropic, InputUSDPerMTok: 3, OutputUSDPerMTok: 15, MaxOutputTokens: opts.MaxOutputTokens,
		}},
		Roles: map[contracts.Role]llm.RoleRoute{contracts.RoleImplementer: {Model: Model}},
	}
	repos := llm.StaticRepos{RepoID: {Tier: contracts.TierPublic, Roles: []contracts.Role{contracts.RoleImplementer}}}
	r, err := llm.New(cfg, map[string]llm.Provider{llm.ProviderAnthropic: provider}, repos, costs)
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	s, err := r.Scope(context.Background(), llm.Scope{
		RepoID: RepoID, FeatureRequestID: FeatureRequestID, AttemptID: AttemptID,
		KeyPrefix: "agenttest", TokenBudget: opts.TokenBudget,
	})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	return s, provider, costs
}
