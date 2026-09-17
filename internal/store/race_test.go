package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// committedFeature inserts a feature request in awaiting_approval and
// commits it, so two connections can contend for the row. Rows are removed
// as the owner when the test ends.
func committedFeature(t *testing.T, ctx context.Context) string {
	t.Helper()
	var repo, feature string
	err := pool.QueryRow(ctx, `INSERT INTO repos (full_name, tier, default_branch)
		VALUES ($1, 'public', 'main') RETURNING id::text`, "race/"+t.Name()).Scan(&repo)
	if err != nil {
		t.Fatalf("seeding repo: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
		VALUES ($1, 1, 'race', 'awaiting_approval', 'feature-race') RETURNING id::text`, repo).Scan(&feature)
	if err != nil {
		t.Fatalf("seeding feature request: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM state_transitions WHERE entity_id = $1`,
			`DELETE FROM feature_requests WHERE id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, feature); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if _, err := pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, repo); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return feature
}

// waitForBlockedUpdate polls until some session is waiting on a row lock in
// an UPDATE of feature_requests, which is the moment the second transaction
// is genuinely queued behind the first.
func waitForBlockedUpdate(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE 'UPDATE feature_requests%'`).Scan(&n)
		if err != nil {
			t.Fatalf("polling pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("second transaction never blocked on the row lock")
}

// contend runs first on one connection, leaves it uncommitted, starts second
// on another connection, waits until it is blocked on the row lock, commits
// the first, and returns what the second saw.
func contend(t *testing.T, ctx context.Context, first, second store.TransitionRequest) error {
	t.Helper()
	txA, err := beginAppTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if err := store.Transition(ctx, txA, first); err != nil {
		t.Fatalf("first transition: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		txB, err := beginAppTx(ctx)
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = txB.Rollback(ctx) }()
		result <- store.Transition(ctx, txB, second)
	}()

	waitForBlockedUpdate(t, ctx)
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit first: %v", err)
	}
	return <-result
}

func committedAudits(t *testing.T, ctx context.Context, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_transitions WHERE entity_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	return n
}

// TestTransitionUnderConcurrentUpdate is the real RL-13 race: two
// connections, the second blocked on the first's uncommitted row lock. When
// the first commits, the second's compare-and-set must see the committed
// state and classify itself correctly. This depends on READ COMMITTED
// re-evaluating the WHERE clause after the lock is released.
func TestTransitionUnderConcurrentUpdate(t *testing.T) {
	cancel := store.TransitionRequest{Kind: contracts.EntityFeatureRequest,
		From: "awaiting_approval", To: "cancelled", Actor: "owner:12345"}
	approve := store.TransitionRequest{Kind: contracts.EntityFeatureRequest,
		From: "awaiting_approval", To: "executing", Actor: "owner:12345"}

	var wrong *store.WrongStateError
	cases := []struct {
		name          string
		first, second store.TransitionRequest
		want          func(error) bool
	}{
		{"cancel commits while approve waits", cancel, approve,
			func(err error) bool { return errors.As(err, &wrong) && wrong.Actual == "cancelled" }},
		{"same request commits while its retry waits", approve, approve,
			func(err error) bool { return errors.Is(err, store.ErrAlreadyApplied) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := requireDB(t)
			id := committedFeature(t, ctx)
			first, second := tc.first, tc.second
			first.ID, second.ID = id, id

			if err := contend(t, ctx, first, second); !tc.want(err) {
				t.Fatalf("second transition: got %v", err)
			}
			// Exactly one committed audit row: the first transition's. The
			// second must not have written one.
			if n := committedAudits(t, ctx, id); n != 1 {
				t.Errorf("committed audit rows = %d, want 1", n)
			}
		})
	}
}
