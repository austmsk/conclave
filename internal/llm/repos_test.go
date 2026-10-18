package llm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// directory is a fake llm.RepoDirectory whose records a test can change
// between requests, as the owner would by re-tiering a repository.
type directory map[string]contracts.Repo

func (d directory) Repo(_ context.Context, id string) (contracts.Repo, bool, error) {
	if id == brokenRepo {
		return contracts.Repo{}, false, errLookup
	}
	r, ok := d[id]
	return r, ok, nil
}

const brokenRepo = "b0b0b0b0-0000-4000-8000-000000000000"

var errLookup = errors.New("database down")

const tallyRepo = "austmsk/election-tally"

var plannerAccess = map[string]llm.RepoAccess{
	tallyRepo: {Tier: contracts.TierSensitive, Roles: []contracts.Role{contracts.RolePlanner}, SensitiveProviders: []string{llm.ProviderAnthropic}},
}

// TestTamperedTierIsRefused attempts what the application's database role
// could do: rewrite a sensitive repository's row to public. The configured
// tier is authoritative, so the row's change does not widen anything; it
// stops every request for the repository, and no provider, not even the
// one the sensitive tier approves, sees one.
func TestTamperedTierIsRefused(t *testing.T) {
	dir := directory{repoID: {ID: repoID, FullName: tallyRepo, Tier: contracts.TierSensitive}}
	cfg := llm.Config{
		Budgets: testBudgets,
		Models:  map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic), "g": model(llm.ProviderGoogle)},
		Roles:   map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m", Fallbacks: []string{"g"}}},
	}
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(endTurn(1, 1), endTurn(1, 1))
	h := newHarness(t, cfg, f, llm.NewDirectoryRepos(dir, plannerAccess), 100000)

	if _, err := complete(h, contracts.RolePlanner); err != nil {
		t.Fatalf("matching record: %v", err)
	}
	dir[repoID] = contracts.Repo{ID: repoID, FullName: tallyRepo, Tier: contracts.TierPublic}
	_, err := complete(h, contracts.RolePlanner)
	if !errors.Is(err, llm.ErrTierMismatch) || !llm.IsPermanent(err) {
		t.Fatalf("tampered record: got %v, want permanent ErrTierMismatch", err)
	}
	if n := len(f[llm.ProviderAnthropic].Requests()); n != 1 {
		t.Fatalf("approved provider received %d requests, want only the first", n)
	}
	if n := len(f[llm.ProviderGoogle].Requests()); n != 0 {
		t.Fatalf("google received %d requests", n)
	}
}

func TestDirectoryReposFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		dir  directory
		want error
	}{
		{"not in the directory", directory{}, llm.ErrUnknownRepo},
		{"no configured policy", directory{repoID: {FullName: "austmsk/other", Tier: contracts.TierSensitive}}, llm.ErrUnknownRepo},
		{"record narrower than configured", directory{repoID: {FullName: tallyRepo, Tier: contracts.TierLocalOnly}}, llm.ErrTierMismatch},
		{"record has no tier", directory{repoID: {FullName: tallyRepo}}, llm.ErrTierMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakes()
			h := newHarness(t, plannerConfig(), f, llm.NewDirectoryRepos(tc.dir, plannerAccess), 100000)
			_, err := complete(h, contracts.RolePlanner)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if n := len(f[llm.ProviderAnthropic].Requests()); n != 0 {
				t.Fatalf("provider received %d requests", n)
			}
		})
	}
}

func TestDirectoryReposValidate(t *testing.T) {
	planner := []contracts.Role{contracts.RolePlanner}
	cases := []struct {
		name   string
		access map[string]llm.RepoAccess
		want   error
	}{
		{"valid", plannerAccess, nil},
		{"no tier", map[string]llm.RepoAccess{"a/b": {Roles: planner}}, llm.ErrNoTier},
		{"unknown tier", map[string]llm.RepoAccess{"a/b": {Tier: "internal", Roles: planner}}, llm.ErrUnknownTier},
		{"no roles", map[string]llm.RepoAccess{"a/b": {Tier: contracts.TierPublic}}, llm.ErrNoRoleAllowlist},
		{"misspelt provider", map[string]llm.RepoAccess{"a/b": {Tier: contracts.TierSensitive, Roles: planner, SensitiveProviders: []string{"anthropc"}}}, llm.ErrUnknownProvider},
		{"empty name", map[string]llm.RepoAccess{"": {Tier: contracts.TierPublic, Roles: planner}}, llm.ErrUnknownRepo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := llm.New(plannerConfig(), providers(fakes()), llm.NewDirectoryRepos(directory{}, tc.access), newCosts())
			if !errors.Is(err, tc.want) {
				t.Fatalf("New: got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDirectoryLookupFailureIsRetryable: a repository missing from the
// directory is permanent, but a lookup that failed is not, so a database
// blip is retried rather than ending the run.
func TestDirectoryLookupFailureIsRetryable(t *testing.T) {
	repos := llm.NewDirectoryRepos(directory{}, plannerAccess)
	_, missing := repos.Policy(context.Background(), repoID)
	_, failed := repos.Policy(context.Background(), brokenRepo)
	if !llm.IsPermanent(missing) {
		t.Fatalf("missing repository %v is not permanent", missing)
	}
	if !errors.Is(failed, errLookup) || llm.IsPermanent(failed) {
		t.Fatalf("failed lookup %v: want the lookup error, retryable", failed)
	}
}
