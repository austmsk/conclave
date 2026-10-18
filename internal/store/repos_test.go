package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

var _ llm.RepoDirectory = (*store.RepoDirectory)(nil)

// TestRetieredRowIsRefused attempts the write the application role is able
// to make: rewriting a repository's tier in the database. The router's
// policy, read through the real directory, follows the owner's
// configuration and refuses the repository once its row disagrees, rather
// than adopting the row's tier (FR-SEC12).
func TestRetieredRowIsRefused(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	repos := llm.NewDirectoryRepos(store.NewRepoDirectory(tx), map[string]llm.RepoAccess{
		"austmsk/election-tally": {Tier: contracts.TierPublic, Roles: []contracts.Role{contracts.RolePlanner}},
	})

	before, err := repos.Policy(ctx, s.repo)
	if err != nil {
		t.Fatal(err)
	}
	if before.Tier != contracts.TierPublic {
		t.Fatalf("tier %q, want public", before.Tier)
	}
	if _, err := tx.Exec(ctx, `UPDATE repos SET tier = 'sensitive' WHERE id = $1`, s.repo); err != nil {
		t.Fatalf("re-tiering: %v", err)
	}
	if _, err := repos.Policy(ctx, s.repo); !errors.Is(err, llm.ErrTierMismatch) {
		t.Fatalf("after re-tiering: got %v, want ErrTierMismatch", err)
	}
}

func TestRepoDirectoryLookup(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	dir := store.NewRepoDirectory(tx)

	got, found, err := dir.Repo(ctx, s.repo)
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	want := contracts.Repo{ID: s.repo, FullName: "austmsk/election-tally", Tier: contracts.TierPublic, DefaultBranch: "main"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// None of these can be a repository, so all are not found rather than
	// errors, and none may abort the transaction: the lookup after them
	// must still work.
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "austmsk/election-tally", "", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"} {
		t.Run(id, func(t *testing.T) {
			_, found, err := dir.Repo(ctx, id)
			if err != nil || found {
				t.Fatalf("found %v, err %v: want not found", found, err)
			}
		})
	}
	if _, found, err := dir.Repo(ctx, s.repo); err != nil || !found {
		t.Fatalf("lookup after bad IDs: found %v, err %v", found, err)
	}
}

// TestCostLedgerRejectsNonUUIDs: an activity that passes, say, a Temporal
// activity ID as the attempt gets ErrInvalidID naming the field, not a
// Postgres syntax error that looks transient.
func TestCostLedgerRejectsNonUUIDs(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)

	reserve := func(attempt, feature string) error {
		_, err := ledger.Reserve(ctx, attempt, feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
			return llm.Claim{}, nil
		})
		return err
	}
	if err := reserve("wf-12/activity-3", s.feature); !errors.Is(err, store.ErrInvalidID) {
		t.Fatalf("Reserve with a bad attempt: got %v, want ErrInvalidID", err)
	}
	if err := reserve(s.attempt, "issue-12"); !errors.Is(err, store.ErrInvalidID) {
		t.Fatalf("Reserve with a bad feature request: got %v, want ErrInvalidID", err)
	}
	if err := ledger.Release(ctx, "reservation-1"); !errors.Is(err, store.ErrInvalidID) {
		t.Fatalf("Release: got %v, want ErrInvalidID", err)
	}
	held := reserveTokens(t, ctx, ledger, s, 1)
	cases := []struct {
		name  string
		apply func(*contracts.CostEvent)
	}{
		{"repository", func(ev *contracts.CostEvent) { ev.RepoID = "repo-1" }},
		{"feature request", func(ev *contracts.CostEvent) { ev.FeatureRequestID = "issue-12" }},
		{"attempt", func(ev *contracts.CostEvent) { ev.AttemptID = "wf-12/activity-3" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := costEvent(s, "k/"+tc.name, 1, 1)
			tc.apply(&ev)
			if err := ledger.Settle(ctx, held, ev); !errors.Is(err, store.ErrInvalidID) {
				t.Fatalf("got %v, want ErrInvalidID", err)
			}
		})
	}
}
