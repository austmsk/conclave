package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// fixture is a committed feature request in awaiting_approval with two
// proposed plan versions, so two connections can contend for the rows.
type fixture struct {
	feature, planA, planB string
}

// committedFixture inserts the fixture as the owner and commits it. Rows are
// removed as the owner when the test ends.
func committedFixture(t *testing.T, ctx context.Context) fixture {
	t.Helper()
	var repo string
	var f fixture
	inserts := []struct {
		dst    *string
		sql    string
		parent *string
	}{
		{&repo, `INSERT INTO repos (full_name, tier, default_branch) VALUES ($1, 'public', 'main') RETURNING id::text`, nil},
		{&f.feature, `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
			VALUES ($1, 1, 'race', 'awaiting_approval', 'feature-race') RETURNING id::text`, &repo},
		{&f.planA, `INSERT INTO plans (feature_request_id, version, state, body) VALUES ($1, 1, 'proposed', '{}') RETURNING id::text`, &f.feature},
		{&f.planB, `INSERT INTO plans (feature_request_id, version, state, body) VALUES ($1, 2, 'proposed', '{}') RETURNING id::text`, &f.feature},
	}
	for _, in := range inserts {
		arg := any("race/" + t.Name())
		if in.parent != nil {
			arg = *in.parent
		}
		if err := pool.QueryRow(ctx, in.sql, arg).Scan(in.dst); err != nil {
			t.Fatalf("seeding fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cleanup := []struct{ sql, id string }{
			{`DELETE FROM state_transitions WHERE entity_id IN ($1, $2, $3)`, ""},
			{`DELETE FROM plans WHERE feature_request_id = $1`, f.feature},
			{`DELETE FROM feature_requests WHERE id = $1`, f.feature},
			{`DELETE FROM repos WHERE id = $1`, repo},
		}
		if _, err := pool.Exec(ctx, cleanup[0].sql, f.feature, f.planA, f.planB); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		for _, c := range cleanup[1:] {
			if _, err := pool.Exec(ctx, c.sql, c.id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	return f
}

// waitForBlockedUpdate polls until some session is waiting on a lock inside
// an UPDATE, which is the moment the second transaction is genuinely queued
// behind the first.
func waitForBlockedUpdate(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE 'UPDATE %'`).Scan(&n)
		if err != nil {
			t.Fatalf("polling pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("second transaction never blocked on a lock")
}

type op func(context.Context, pgx.Tx) error

// contend runs first on one connection and leaves it uncommitted, starts
// second on another connection, waits until it is blocked on a lock the
// first holds, commits the first, and returns what the second saw.
func contend(t *testing.T, ctx context.Context, first, second op) error {
	t.Helper()
	txA, err := beginAppTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if err := first(ctx, txA); err != nil {
		t.Fatalf("first: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		txB, err := beginAppTx(ctx)
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = txB.Rollback(ctx) }()
		result <- second(ctx, txB)
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

func transitionOp(req store.TransitionRequest) op {
	return func(ctx context.Context, tx pgx.Tx) error { return store.Transition(ctx, tx, req) }
}

// TestTransitionUnderConcurrentUpdate is the real RL-13 race: two
// connections, the second blocked on the first's uncommitted row lock. When
// the first commits, the second's compare-and-set must see the committed
// state and classify itself correctly. This depends on READ COMMITTED
// re-evaluating the WHERE clause after the lock is released.
func TestTransitionUnderConcurrentUpdate(t *testing.T) {
	cancel := store.TransitionRequest{Kind: contracts.EntityFeatureRequest,
		From: "awaiting_approval", To: "cancelled", Revision: 0, Actor: "owner:12345"}
	approve := store.TransitionRequest{Kind: contracts.EntityFeatureRequest,
		From: "awaiting_approval", To: "executing", Revision: 0, Actor: "owner:12345"}

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
			f := committedFixture(t, ctx)
			first, second := tc.first, tc.second
			first.ID, second.ID = f.feature, f.feature

			if err := contend(t, ctx, transitionOp(first), transitionOp(second)); !tc.want(err) {
				t.Fatalf("second transition: got %v", err)
			}
			// Exactly one committed audit row: the first transition's. The
			// second must not have written one.
			if n := committedAudits(t, ctx, f.feature); n != 1 {
				t.Errorf("committed audit rows = %d, want 1", n)
			}
		})
	}
}

// TestApprovePlanUnderConcurrentApproval approves two versions of the same
// plan on two connections. The second blocks on the partial unique index
// until the first commits, then must report ErrAnotherPlanApproved rather
// than an opaque constraint error.
func TestApprovePlanUnderConcurrentApproval(t *testing.T) {
	ctx := requireDB(t)
	f := committedFixture(t, ctx)

	approve := func(plan, sha string) op {
		return func(ctx context.Context, tx pgx.Tx) error {
			return store.ApprovePlan(ctx, tx, plan, 0, sha, "owner:12345")
		}
	}
	err := contend(t, ctx, approve(f.planA, "aaaa"), approve(f.planB, "bbbb"))
	if !errors.Is(err, store.ErrAnotherPlanApproved) {
		t.Fatalf("second approval: got %v, want ErrAnotherPlanApproved", err)
	}
	if n := committedAudits(t, ctx, f.planA); n != 1 {
		t.Errorf("audit rows for winning plan = %d, want 1", n)
	}
	if n := committedAudits(t, ctx, f.planB); n != 0 {
		t.Errorf("audit rows for losing plan = %d, want 0", n)
	}
}
