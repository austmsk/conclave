package store_test

import (
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

var _ llm.CostLedger = (*store.CostLedger)(nil)

func costEvent(s ids, key string, input, output int) contracts.CostEvent {
	return contracts.CostEvent{
		IdempotencyKey: key, RepoID: s.repo, FeatureRequestID: s.feature, AttemptID: s.attempt,
		Role: contracts.RoleImplementer, Provider: "anthropic", Model: "claude-fable-5-1",
		InputTokens: input, OutputTokens: output, CostUSD: 0.01,
		OccurredAt: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
	}
}

// TestRecordCostIsIdempotent replays a write the way a retried activity
// does: the same key twice, the second time with different numbers. The
// second insert must succeed, leave the transaction usable, and change
// nothing, so the spend is counted once (RL-2). It runs as the application
// role, whose only rights on cost_events are SELECT and INSERT, so it also
// shows ON CONFLICT DO NOTHING needs no UPDATE.
func TestRecordCostIsIdempotent(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)

	if err := ledger.RecordCost(ctx, costEvent(s, "wf/act/anthropic/msg_1", 100, 50)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := ledger.RecordCost(ctx, costEvent(s, "wf/act/anthropic/msg_1", 999, 999)); err != nil {
		t.Fatalf("repeated write: %v", err)
	}
	var rows int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM cost_events WHERE idempotency_key = 'wf/act/anthropic/msg_1'`).Scan(&rows); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one key, want 1", rows)
	}
	got, err := ledger.AttemptTokens(ctx, s.attempt)
	if err != nil {
		t.Fatal(err)
	}
	if got != 150 {
		t.Fatalf("AttemptTokens = %d, want 150 from the first write", got)
	}
}

// TestAttemptTokens sums one attempt's events and nothing else.
func TestAttemptTokens(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	other, err := insertInState(ctx, tx, s, contracts.EntityAttempt, "running")
	if err != nil {
		t.Fatalf("seeding second attempt: %v", err)
	}
	ledger := store.NewCostLedger(tx)

	events := []contracts.CostEvent{
		costEvent(s, "a/1", 100, 10),
		costEvent(s, "a/2", 200, 20),
	}
	elsewhere := costEvent(s, "b/1", 5000, 5000)
	elsewhere.AttemptID = other
	unattributed := costEvent(s, "c/1", 7000, 7000)
	unattributed.AttemptID, unattributed.FeatureRequestID, unattributed.OccurredAt = "", "", time.Time{}
	for _, ev := range append(events, elsewhere, unattributed) {
		if err := ledger.RecordCost(ctx, ev); err != nil {
			t.Fatalf("recording %s: %v", ev.IdempotencyKey, err)
		}
	}

	cases := []struct {
		name    string
		attempt string
		want    int
	}{
		{"this attempt", s.attempt, 330},
		{"other attempt", other, 10000},
		{"attempt with no spend", "00000000-0000-0000-0000-000000000000", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ledger.AttemptTokens(ctx, tc.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("AttemptTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRecordCostRequiresKey(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	if err := store.NewCostLedger(tx).RecordCost(ctx, costEvent(s, "", 1, 1)); err == nil {
		t.Fatal("recorded an event with no idempotency key")
	}
}
