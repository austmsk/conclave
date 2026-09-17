package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

func stateOf(t *testing.T, ctx context.Context, tx pgx.Tx, table, id string) (state, reason string) {
	t.Helper()
	err := tx.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM `+table+` WHERE id = $1`, id).Scan(&state, &reason)
	if err != nil {
		t.Fatalf("reading %s %s: %v", table, id, err)
	}
	return state, reason
}

func auditRows(t *testing.T, ctx context.Context, tx pgx.Tx, id string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM state_transitions WHERE entity_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	return n
}

func TestTransitionAppliesAndRecordsAudit(t *testing.T) {
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

	var kind, from, to, actor, reason string
	err = tx.QueryRow(ctx, `SELECT entity_kind, from_state, to_state, actor, reason
		FROM state_transitions WHERE entity_id = $1`, s.task).Scan(&kind, &from, &to, &actor, &reason)
	if err != nil {
		t.Fatalf("reading audit row: %v", err)
	}
	if kind != "task" || from != "running" || to != "needs_attention" || actor != "workflow:feature-7" || reason != "no attempt passed" {
		t.Errorf("audit row = (%s, %s, %s, %s, %s)", kind, from, to, actor, reason)
	}
}

// TestTransitionDistinguishesRetryFromRace is the heart of RL-13. Two
// zero-row updates look identical to the database; the helper must report
// one as "already done" and the other as "someone else got here first".
func TestTransitionDistinguishesRetryFromRace(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	finish := store.TransitionRequest{
		Kind: contracts.EntityAttempt, ID: s.attempt,
		From: "running", To: "passed", Actor: "activity:gate",
	}
	if err := store.Transition(ctx, tx, finish); err != nil {
		t.Fatalf("first transition: %v", err)
	}

	// A retried activity replays the same request.
	err := store.Transition(ctx, tx, finish)
	if !errors.Is(err, store.ErrAlreadyApplied) {
		t.Fatalf("retry: got %v, want ErrAlreadyApplied", err)
	}
	if n := auditRows(t, ctx, tx, s.attempt); n != 1 {
		t.Errorf("audit rows after retry = %d, want 1", n)
	}

	// A different outcome for the same attempt lost the race.
	race := finish
	race.To = "failed"
	err = store.Transition(ctx, tx, race)
	var wrong *store.WrongStateError
	if !errors.As(err, &wrong) {
		t.Fatalf("race: got %v, want WrongStateError", err)
	}
	if wrong.Expected != "running" || wrong.Actual != "passed" {
		t.Errorf("WrongStateError = %+v", wrong)
	}
	if errors.Is(err, store.ErrAlreadyApplied) {
		t.Error("a race must not be reported as already applied")
	}
}

// TestTransitionCancelDuringApproval is the documented race: /cancel lands
// while /approve is being processed. The approval must fail loudly.
func TestTransitionCancelDuringApproval(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	steps := []store.TransitionRequest{
		{Kind: contracts.EntityFeatureRequest, ID: s.feature, From: "planning", To: "awaiting_approval", Actor: "workflow:feature-7"},
		{Kind: contracts.EntityFeatureRequest, ID: s.feature, From: "awaiting_approval", To: "cancelled", Actor: "owner:12345"},
	}
	for _, req := range steps {
		if err := store.Transition(ctx, tx, req); err != nil {
			t.Fatalf("%s -> %s: %v", req.From, req.To, err)
		}
	}

	approve := store.TransitionRequest{Kind: contracts.EntityFeatureRequest, ID: s.feature,
		From: "awaiting_approval", To: "executing", Actor: "owner:12345"}
	err := store.Transition(ctx, tx, approve)
	var wrong *store.WrongStateError
	if !errors.As(err, &wrong) || wrong.Actual != "cancelled" {
		t.Fatalf("approve after cancel: got %v, want WrongStateError with Actual=cancelled", err)
	}
	if state, _ := stateOf(t, ctx, tx, "feature_requests", s.feature); state != "cancelled" {
		t.Errorf("feature request state = %q, want cancelled", state)
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
			From: "running", To: "running", Actor: "test"},
			func(err error) bool { return errors.As(err, &illegal) }},
		{"unknown kind", store.TransitionRequest{Kind: "widget", ID: s.attempt,
			From: "running", To: "passed", Actor: "test"},
			func(err error) bool { return errors.As(err, &illegal) }},
		{"missing actor", store.TransitionRequest{Kind: contracts.EntityAttempt, ID: s.attempt,
			From: "running", To: "passed"},
			func(err error) bool { return err != nil && !errors.As(err, &illegal) }},
		{"not found", store.TransitionRequest{Kind: contracts.EntityAttempt,
			ID: "00000000-0000-0000-0000-000000000000", From: "running", To: "passed", Actor: "test"},
			func(err error) bool { return errors.Is(err, store.ErrNotFound) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Transition(ctx, tx, tc.req)
			if !tc.want(err) {
				t.Fatalf("got %v", err)
			}
			if state, _ := stateOf(t, ctx, tx, "attempts", s.attempt); state != "running" {
				t.Errorf("attempt state changed to %q", state)
			}
			if n := auditRows(t, ctx, tx, s.attempt); n != 0 {
				t.Errorf("audit rows = %d, want 0", n)
			}
		})
	}
}
