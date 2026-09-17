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

type audit struct {
	kind, from, to, actor, reason string
}

// audits returns every audit row for the entity, oldest first.
func audits(t *testing.T, ctx context.Context, tx pgx.Tx, id string) []audit {
	t.Helper()
	rows, err := tx.Query(ctx, `SELECT entity_kind, from_state, to_state, actor, coalesce(reason, '')
		FROM state_transitions WHERE entity_id = $1 ORDER BY occurred_at, id`, id)
	if err != nil {
		t.Fatalf("reading audit rows: %v", err)
	}
	defer rows.Close()
	var out []audit
	for rows.Next() {
		var a audit
		if err := rows.Scan(&a.kind, &a.from, &a.to, &a.actor, &a.reason); err != nil {
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

// move is the production path for a transition. Plan approval is the one
// move with its own entry point because it pins the base commit.
func move(ctx context.Context, tx pgx.Tx, kind contracts.EntityKind, id, from, to string) error {
	if kind == contracts.EntityPlan && to == string(contracts.PlanApproved) {
		return store.ApprovePlan(ctx, tx, id, "abc123", testActor)
	}
	return store.Transition(ctx, tx, store.TransitionRequest{
		Kind: kind, ID: id, From: from, To: to, Actor: testActor, Reason: "because",
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
	want := []audit{{kind: string(m.kind), from: m.from, to: m.to, actor: testActor}}
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
		From: "running", To: "needs_attention",
		Actor: "workflow:feature-7", Reason: "no attempt passed",
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if state, reason := stateOf(t, ctx, tx, "tasks", s.task); state != "needs_attention" || reason != "no attempt passed" {
		t.Errorf("task = (%q, %q), want (needs_attention, no attempt passed)", state, reason)
	}
	want := []audit{{"task", "running", "needs_attention", "workflow:feature-7", "no attempt passed"}}
	if got := audits(t, ctx, tx, s.task); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit rows = %+v, want %+v", got, want)
	}
}

// TestTransitionReplayInOneTransaction covers the sequential half of RL-13:
// the same request twice, then a conflicting one, all on one connection.
// The concurrent half lives in race_test.go.
func TestTransitionReplayInOneTransaction(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	finish := store.TransitionRequest{
		Kind: contracts.EntityAttempt, ID: s.attempt,
		From: "running", To: "passed", Actor: "activity:gate",
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

	race := finish
	race.To = "failed"
	err = store.Transition(ctx, tx, race)
	var wrong *store.WrongStateError
	if !errors.As(err, &wrong) {
		t.Fatalf("conflict: got %v, want WrongStateError", err)
	}
	if wrong.Expected != "running" || wrong.Actual != "passed" {
		t.Errorf("WrongStateError = %+v", wrong)
	}
	if errors.Is(err, store.ErrAlreadyApplied) {
		t.Error("a conflict must not be reported as already applied")
	}
}

func TestTransitionRefusals(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	var illegal *store.IllegalTransitionError
	cases := []struct {
		name string
		req  store.TransitionRequest
		want func(error) bool
	}{
		{"illegal move", store.TransitionRequest{Kind: contracts.EntityAttempt, ID: s.attempt,
			From: "running", To: "running", Actor: testActor},
			func(err error) bool { return errors.As(err, &illegal) }},
		{"unknown kind", store.TransitionRequest{Kind: "widget", ID: s.attempt,
			From: "running", To: "passed", Actor: testActor},
			func(err error) bool { return errors.As(err, &illegal) }},
		{"missing actor", store.TransitionRequest{Kind: contracts.EntityAttempt, ID: s.attempt,
			From: "running", To: "passed"},
			func(err error) bool { return err != nil && !errors.As(err, &illegal) }},
		{"not found", store.TransitionRequest{Kind: contracts.EntityAttempt,
			ID: "00000000-0000-0000-0000-000000000000", From: "running", To: "passed", Actor: testActor},
			func(err error) bool { return errors.Is(err, store.ErrNotFound) }},
		{"plan approval through the generic helper", store.TransitionRequest{Kind: contracts.EntityPlan,
			ID: s.plan, From: "proposed", To: "approved", Actor: testActor},
			func(err error) bool { return errors.Is(err, store.ErrApprovalNeedsBaseSHA) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Transition(ctx, tx, tc.req)
			if !tc.want(err) {
				t.Fatalf("got %v", err)
			}
			for _, kind := range []contracts.EntityKind{contracts.EntityAttempt, contracts.EntityPlan} {
				id := map[contracts.EntityKind]string{contracts.EntityAttempt: s.attempt, contracts.EntityPlan: s.plan}[kind]
				if n := len(audits(t, ctx, tx, id)); n != 0 {
					t.Errorf("%s audit rows = %d, want 0", kind, n)
				}
			}
			if state, _ := stateOf(t, ctx, tx, "attempts", s.attempt); state != "running" {
				t.Errorf("attempt state changed to %q", state)
			}
		})
	}
}

// TestDirectStateUpdateIsRefused attempts the bypass: application code that
// updates a state column without going through the helper. The trigger
// refuses it, so an unaudited state change is impossible for the
// application role rather than merely discouraged (FR-SEC10).
func TestDirectStateUpdateIsRefused(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	sp := savepoint(t, ctx, tx)
	_, err := sp.Exec(ctx, `UPDATE attempts SET state = 'passed' WHERE id = $1`, s.attempt)
	if got := sqlState(err); got != store.SQLStateUnattributedStateChange {
		t.Fatalf("direct update: got %v (SQLSTATE %q), want SQLSTATE %s", err, got, store.SQLStateUnattributedStateChange)
	}
	_ = sp.Rollback(ctx)

	if state, _ := stateOf(t, ctx, tx, "attempts", s.attempt); state != "running" {
		t.Errorf("attempt state = %q after refused update, want running", state)
	}
	if n := len(audits(t, ctx, tx, s.attempt)); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}

	// Updates that leave state alone are unaffected.
	if _, err := tx.Exec(ctx, `UPDATE attempts SET steps = 3 WHERE id = $1`, s.attempt); err != nil {
		t.Errorf("non-state update: %v", err)
	}
}
