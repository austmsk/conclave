package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

const testActor = "test:transition"

func stateOf(t *testing.T, ctx context.Context, tx pgx.Tx, table, id string) (state, reason string) {
	t.Helper()
	err := tx.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM `+table+` WHERE id = $1`, id).Scan(&state, &reason)
	if err != nil {
		t.Fatalf("reading %s %s: %v", table, id, err)
	}
	return state, reason
}

func revisionOf(t *testing.T, ctx context.Context, tx pgx.Tx, table, id string) int {
	t.Helper()
	var rev int
	if err := tx.QueryRow(ctx, `SELECT revision FROM `+table+` WHERE id = $1`, id).Scan(&rev); err != nil {
		t.Fatalf("reading revision of %s %s: %v", table, id, err)
	}
	return rev
}

func sessionUser(t *testing.T, ctx context.Context, tx pgx.Tx) string {
	t.Helper()
	var u string
	if err := tx.QueryRow(ctx, `SELECT session_user`).Scan(&u); err != nil {
		t.Fatalf("reading session_user: %v", err)
	}
	return u
}

type audit struct {
	kind, from, to, actor, dbUser, reason string
	revision                              int
}

// audits returns every audit row for the entity, oldest first. The table is
// schema-qualified so a temp table can never satisfy this helper.
func audits(t *testing.T, ctx context.Context, tx pgx.Tx, id string) []audit {
	t.Helper()
	rows, err := tx.Query(ctx, `SELECT entity_kind, from_state, to_state, actor, db_user, coalesce(reason, ''), revision
		FROM public.state_transitions WHERE entity_id = $1 ORDER BY revision`, id)
	if err != nil {
		t.Fatalf("reading audit rows: %v", err)
	}
	defer rows.Close()
	var out []audit
	for rows.Next() {
		var a audit
		if err := rows.Scan(&a.kind, &a.from, &a.to, &a.actor, &a.dbUser, &a.reason, &a.revision); err != nil {
			t.Fatalf("scanning audit row: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// tableFor mirrors the kind-to-table mapping so tests can read state back
// without reaching into the package.
func tableFor(kind contracts.EntityKind) string {
	return map[contracts.EntityKind]string{
		contracts.EntityFeatureRequest: "feature_requests",
		contracts.EntityPlan:           "plans",
		contracts.EntityTask:           "tasks",
		contracts.EntityAttempt:        "attempts",
	}[kind]
}

// move is the production path for a transition of a freshly seeded record
// (revision 0). Plan approval is the one move with its own entry point
// because it pins the base commit.
func move(ctx context.Context, tx pgx.Tx, kind contracts.EntityKind, id, from, to string) error {
	if kind == contracts.EntityPlan && to == string(contracts.PlanApproved) {
		return store.ApprovePlan(ctx, tx, id, 0, "abc123", testActor)
	}
	return store.Transition(ctx, tx, store.TransitionRequest{
		Kind: kind, ID: id, From: from, To: to, Revision: 0, Actor: testActor, Reason: "because",
	})
}

type legalMove struct {
	kind     contracts.EntityKind
	from, to string
}

// legalMoves enumerates the transition table: every kind, every ordered
// pair of its states, filtered to the legal ones.
func legalMoves() []legalMove {
	var moves []legalMove
	for _, kind := range contracts.EntityKinds() {
		states := contracts.States(kind)
		for _, from := range states {
			for _, to := range states {
				if contracts.CanTransition(kind, from, to) {
					moves = append(moves, legalMove{kind, from, to})
				}
			}
		}
	}
	return moves
}

// TestTransitionCoversEveryLegalMove drives every legal move of every entity
// kind through the production helpers, not through hand-written SQL. The
// matrix comes from the contracts transition table, so a kind or state added
// there is exercised here automatically, and a kind the seed helper does not
// know fails the test.
func TestTransitionCoversEveryLegalMove(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	for _, m := range legalMoves() {
		t.Run(fmt.Sprintf("%s/%s->%s", m.kind, m.from, m.to), func(t *testing.T) {
			checkMove(t, ctx, savepoint(t, ctx, tx), s, m)
		})
	}
}

func checkMove(t *testing.T, ctx context.Context, tx pgx.Tx, s ids, m legalMove) {
	t.Helper()
	id, err := insertInState(ctx, tx, s, m.kind, m.from)
	if err != nil {
		t.Fatalf("seeding %s in %q: %v", m.kind, m.from, err)
	}
	if err := move(ctx, tx, m.kind, id, m.from, m.to); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got, _ := stateOf(t, ctx, tx, tableFor(m.kind), id); got != m.to {
		t.Errorf("state = %q, want %q", got, m.to)
	}
	if rev := revisionOf(t, ctx, tx, tableFor(m.kind), id); rev != 1 {
		t.Errorf("revision = %d, want 1", rev)
	}
	want := []audit{{kind: string(m.kind), from: m.from, to: m.to, actor: testActor,
		dbUser: sessionUser(t, ctx, tx), revision: 1}}
	got := audits(t, ctx, tx, id)
	for i := range got {
		got[i].reason = "" // reason varies by path; checked in TestTransitionRecordsReason
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit rows = %+v, want %+v", got, want)
	}
}

func TestTransitionRecordsReason(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	err := store.Transition(ctx, tx, store.TransitionRequest{
		Kind: contracts.EntityTask, ID: s.task,
		From: "running", To: "needs_attention", Revision: 0,
		Actor: "workflow:feature-7", Reason: "no attempt passed",
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if state, reason := stateOf(t, ctx, tx, "tasks", s.task); state != "needs_attention" || reason != "no attempt passed" {
		t.Errorf("task = (%q, %q), want (needs_attention, no attempt passed)", state, reason)
	}
	want := []audit{{"task", "running", "needs_attention", "workflow:feature-7", sessionUser(t, ctx, tx), "no attempt passed", 1}}
	if got := audits(t, ctx, tx, s.task); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit rows = %+v, want %+v", got, want)
	}
}

// TestTransitionReplayInOneTransaction covers the sequential half of RL-13
// on an acyclic lifecycle: the same request twice, then a conflicting one.
// The concurrent half lives in race_test.go.
func TestTransitionReplayInOneTransaction(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	finish := store.TransitionRequest{
		Kind: contracts.EntityAttempt, ID: s.attempt,
		From: "running", To: "passed", Revision: 0, Actor: "activity:gate",
	}
	if err := store.Transition(ctx, tx, finish); err != nil {
		t.Fatalf("first transition: %v", err)
	}

	err := store.Transition(ctx, tx, finish)
	if !errors.Is(err, store.ErrAlreadyApplied) {
		t.Fatalf("retry: got %v, want ErrAlreadyApplied", err)
	}
	if n := len(audits(t, ctx, tx, s.attempt)); n != 1 {
		t.Errorf("audit rows after retry = %d, want 1", n)
	}

	conflict := finish
	conflict.To = "failed"
	err = store.Transition(ctx, tx, conflict)
	var wrong *store.WrongStateError
	if !errors.As(err, &wrong) {
		t.Fatalf("conflict: got %v, want WrongStateError", err)
	}
	if wrong.Expected != "running" || wrong.Actual != "passed" || wrong.ExpectedRevision != 0 || wrong.ActualRevision != 1 {
		t.Errorf("WrongStateError = %+v", wrong)
	}
	if errors.Is(err, store.ErrAlreadyApplied) {
		t.Error("a conflict must not be reported as already applied")
	}
}

// TestTransitionStaleRetryOnCyclicLifecycle is the case state-only
// compare-and-set gets wrong. A task fails, the owner retries it back to
// running, and then Temporal replays the original failing activity. Its
// From still matches the current state; only the revision says it is stale.
func TestTransitionStaleRetryOnCyclicLifecycle(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	fail := store.TransitionRequest{Kind: contracts.EntityTask, ID: s.task,
		From: "running", To: "needs_attention", Revision: 0, Actor: "activity:gate", Reason: "no attempt passed"}
	retry := store.TransitionRequest{Kind: contracts.EntityTask, ID: s.task,
		From: "needs_attention", To: "running", Revision: 1, Actor: "owner:12345"}
	for _, req := range []store.TransitionRequest{fail, retry} {
		if err := store.Transition(ctx, tx, req); err != nil {
			t.Fatalf("%s -> %s: %v", req.From, req.To, err)
		}
	}

	err := store.Transition(ctx, tx, fail)
	var wrong *store.WrongStateError
	if !errors.As(err, &wrong) {
		t.Fatalf("stale replay: got %v, want WrongStateError", err)
	}
	if wrong.Actual != "running" || wrong.ActualRevision != 2 {
		t.Errorf("WrongStateError = %+v, want running at revision 2", wrong)
	}
	if state, _ := stateOf(t, ctx, tx, "tasks", s.task); state != "running" {
		t.Errorf("stale replay changed state to %q", state)
	}
	if n := len(audits(t, ctx, tx, s.task)); n != 2 {
		t.Errorf("audit rows = %d, want 2", n)
	}

	// The owner's own request replayed is the genuine already-applied case.
	if err := store.Transition(ctx, tx, retry); !errors.Is(err, store.ErrAlreadyApplied) {
		t.Errorf("replaying the latest transition: got %v, want ErrAlreadyApplied", err)
	}
}

func TestTransitionRefusals(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	var illegal *store.IllegalTransitionError
	var wrong *store.WrongStateError
	attempt := store.TransitionRequest{Kind: contracts.EntityAttempt, ID: s.attempt,
		From: "running", To: "passed", Revision: 0, Actor: testActor}
	with := func(f func(*store.TransitionRequest)) store.TransitionRequest {
		r := attempt
		f(&r)
		return r
	}
	cases := []struct {
		name string
		req  store.TransitionRequest
		want func(error) bool
	}{
		{"illegal move", with(func(r *store.TransitionRequest) { r.To = "running" }),
			func(err error) bool { return errors.As(err, &illegal) }},
		{"unknown kind", with(func(r *store.TransitionRequest) { r.Kind = "widget" }),
			func(err error) bool { return errors.As(err, &illegal) }},
		{"missing actor", with(func(r *store.TransitionRequest) { r.Actor = "" }),
			func(err error) bool { return err != nil && !errors.As(err, &illegal) }},
		{"negative revision", with(func(r *store.TransitionRequest) { r.Revision = -1 }),
			func(err error) bool { return err != nil && !errors.As(err, &illegal) && !errors.As(err, &wrong) }},
		{"stale revision", with(func(r *store.TransitionRequest) { r.Revision = 5 }),
			func(err error) bool { return errors.As(err, &wrong) && wrong.ActualRevision == 0 }},
		{"not found", with(func(r *store.TransitionRequest) { r.ID = "00000000-0000-0000-0000-000000000000" }),
			func(err error) bool { return errors.Is(err, store.ErrNotFound) }},
		{"plan approval through the generic helper", store.TransitionRequest{Kind: contracts.EntityPlan,
			ID: s.plan, From: "proposed", To: "approved", Actor: testActor},
			func(err error) bool { return errors.Is(err, store.ErrApprovalNeedsBaseSHA) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.Transition(ctx, tx, tc.req); !tc.want(err) {
				t.Fatalf("got %v", err)
			}
			for _, id := range []string{s.attempt, s.plan} {
				if n := len(audits(t, ctx, tx, id)); n != 0 {
					t.Errorf("audit rows for %s = %d, want 0", id, n)
				}
			}
			if state, _ := stateOf(t, ctx, tx, "attempts", s.attempt); state != "running" {
				t.Errorf("attempt state changed to %q", state)
			}
		})
	}
}
