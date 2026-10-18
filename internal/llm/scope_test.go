package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// Another feature request and attempt the test ledger knows, so a scope can
// name real IDs that are linked to each other but not to the test scope's.
const (
	otherFeatureID = "5e4d3c2b-1a0f-4e9d-8c7b-6a5f4e3d2c1b"
	otherAttemptID = "9b8a7f6e-5d4c-4b3a-9281-7f6e5d4c3b2a"
)

func validScope() llm.Scope {
	return llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 10}
}

// scopeRouter is a router whose ledger knows the test attempt and a second
// attempt in a second feature request of the same repository.
func scopeRouter(t *testing.T) *llm.Router {
	t.Helper()
	costs := newCosts()
	costs.Known = append(costs.Known, faketest.Attribution{RepoID: repoID, FeatureRequestID: otherFeatureID, AttemptID: otherAttemptID})
	r, err := llm.New(plannerConfig(), providers(fakes()), publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestScopeRequiresAttribution changes one field of a valid attempt scope
// per case, and checks the refusal names that field, so no case passes on
// another case's check. The attribution cases attempt the dodge 8a closes:
// charging an attempt's spend to a feature request it does not belong to,
// where that feature request's cap would not see it.
func TestScopeRequiresAttribution(t *testing.T) {
	r := scopeRouter(t)
	if _, err := r.Scope(context.Background(), validScope()); err != nil {
		t.Fatalf("valid scope refused: %v", err)
	}
	cases := []struct {
		name   string
		change func(*llm.Scope)
		want   string
	}{
		{"no repository", func(s *llm.Scope) { s.RepoID = "" }, "no repository"},
		{"no feature request", func(s *llm.Scope) { s.FeatureRequestID = "" }, "no feature request"},
		{"no attempt", func(s *llm.Scope) { s.AttemptID = "" }, "no attempt"},
		{"no key prefix", func(s *llm.Scope) { s.KeyPrefix = "" }, "empty key prefix"},
		{"no budget", func(s *llm.Scope) { s.TokenBudget = 0 }, "budget 0"},
		{"negative budget", func(s *llm.Scope) { s.TokenBudget = -1 }, "budget -1"},
		// IDs that are not UUIDs, which the ledger would refuse only after
		// a reply had been billed.
		{"repository not a UUID", func(s *llm.Scope) { s.RepoID = "repo-1" }, "not a UUID"},
		{"feature request not a UUID", func(s *llm.Scope) { s.FeatureRequestID = "issue-12" }, "not a UUID"},
		{"attempt not a UUID", func(s *llm.Scope) { s.AttemptID = "wf-12/activity-3" }, "not a UUID"},
		{"attempt with a misplaced dash", func(s *llm.Scope) { s.AttemptID = "7a1d9e44x2b3c-4f5a-8e6d-0c1b2a3f4e5d" }, "not a UUID"},
		{"attempt of another feature request", func(s *llm.Scope) { s.AttemptID = otherAttemptID }, "is not in feature request"},
		{"feature request of another attempt", func(s *llm.Scope) { s.FeatureRequestID = otherFeatureID }, "is not in feature request"},
		{"unknown attempt", func(s *llm.Scope) { s.AttemptID = "00000000-0000-4000-8000-000000000000" }, "is not in feature request"},
		{"another repository", func(s *llm.Scope) { s.RepoID = "00000000-0000-4000-8000-000000000001" }, "is not in feature request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validScope()
			tc.change(&s)
			_, err := r.Scope(context.Background(), s)
			if !errors.Is(err, llm.ErrInvalidScope) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want ErrInvalidScope saying %q", err, tc.want)
			}
		})
	}
}

// TestFeatureRequestScope: calls outside an attempt need a feature request
// in the scope's repository, and may not carry an attempt or a budget, so
// attempt work cannot use this constructor to escape its attempt's budget.
func TestFeatureRequestScope(t *testing.T) {
	r := scopeRouter(t)
	valid := llm.Scope{RepoID: repoID, FeatureRequestID: featureID, KeyPrefix: "k"}
	if _, err := r.FeatureRequestScope(context.Background(), valid); err != nil {
		t.Fatalf("valid scope refused: %v", err)
	}
	cases := []struct {
		name   string
		change func(*llm.Scope)
		want   string
	}{
		{"with an attempt", func(s *llm.Scope) { s.AttemptID = attemptID }, "use Scope"},
		{"with a budget", func(s *llm.Scope) { s.TokenBudget = 1000 }, "no budget of its own"},
		{"no feature request", func(s *llm.Scope) { s.FeatureRequestID = "" }, "no feature request"},
		{"no key prefix", func(s *llm.Scope) { s.KeyPrefix = "" }, "empty key prefix"},
		{"unknown feature request", func(s *llm.Scope) { s.FeatureRequestID = "00000000-0000-4000-8000-000000000000" }, "is not in feature request"},
		{"another repository", func(s *llm.Scope) { s.RepoID = "00000000-0000-4000-8000-000000000001" }, "is not in feature request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.change(&s)
			_, err := r.FeatureRequestScope(context.Background(), s)
			if !errors.Is(err, llm.ErrInvalidScope) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want ErrInvalidScope saying %q", err, tc.want)
			}
		})
	}
}

// TestFeatureRequestScopeIsCapped: a call outside an attempt is charged to
// its feature request, with no attempt, and the feature request cap refuses
// it like any other call.
func TestFeatureRequestScopeIsCapped(t *testing.T) {
	cfg := plannerConfig()
	cfg.Budgets.FeatureRequestTokens = 300
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(endTurn(150, 150), endTurn(1, 1))
	costs := newCosts()
	r, err := llm.New(cfg, providers(f), publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.FeatureRequestScope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, KeyPrefix: "analysis"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(context.Background(), plannerRequest(100)); err != nil {
		t.Fatal(err)
	}
	evs := costs.Events()
	if len(evs) != 1 || evs[0].FeatureRequestID != featureID || evs[0].AttemptID != "" {
		t.Fatalf("events %+v, want one charged to the feature request and no attempt", evs)
	}
	_, err = s.Complete(context.Background(), plannerRequest(100))
	assertBudgetKind(t, err, llm.BudgetFeatureRequest)
	if f[llm.ProviderAnthropic].Remaining() != 1 {
		t.Fatal("the refused call reached the provider")
	}
}

// assertBudgetKind checks everything a caller relies on in a cap refusal.
func assertBudgetKind(t *testing.T, err error, want llm.BudgetKind) {
	t.Helper()
	var be llm.BudgetError
	if !errors.As(err, &be) || be.Kind != want {
		t.Fatalf("got %v, want a BudgetError of kind %s", err, want)
	}
	if !errors.Is(err, llm.ErrBudgetExceeded) || !llm.IsPermanent(err) {
		t.Fatalf("%v: errors.Is ErrBudgetExceeded %v, IsPermanent %v, want both",
			err, errors.Is(err, llm.ErrBudgetExceeded), llm.IsPermanent(err))
	}
}
