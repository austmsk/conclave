package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// reserveTokens reserves a claim of tokens for the seed attempt and returns
// its ID.
func reserveTokens(t *testing.T, ctx context.Context, ledger *store.CostLedger, s ids, tokens int) string {
	t.Helper()
	id, err := ledger.Reserve(ctx, s.attempt, s.feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
		return llm.Claim{Tokens: tokens}, nil
	})
	if err != nil {
		t.Fatalf("reserving: %v", err)
	}
	return id
}

func countRows(t *testing.T, ctx context.Context, tx pgx.Tx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestSettleIsIdempotent replays a settlement the way a retried activity
// does: the same key twice, the second time with different numbers. The
// second must succeed, leave the transaction usable, and change nothing,
// so the spend is counted once (RL-2). It runs as the application role,
// whose only rights on cost_events are SELECT and INSERT, so it also shows
// ON CONFLICT DO NOTHING needs no UPDATE.
func TestSettleIsIdempotent(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	held := reserveTokens(t, ctx, ledger, s, 500)

	if err := ledger.Settle(ctx, held, costEvent(s, "wf/act/anthropic/msg_1", 100, 50)); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if err := ledger.Settle(ctx, held, costEvent(s, "wf/act/anthropic/msg_1", 999, 999)); err != nil {
		t.Fatalf("repeated settle: %v", err)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM cost_events WHERE idempotency_key = 'wf/act/anthropic/msg_1'`); n != 1 {
		t.Fatalf("%d rows for one key, want 1", n)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM budget_reservations`); n != 0 {
		t.Fatalf("%d reservations after settling, want 0", n)
	}
	sp := spendNow(t, ctx, ledger, s.attempt, s.feature)
	if sp.AttemptTokens != 150 {
		t.Fatalf("attempt spend %d, want 150 from the first settle alone", sp.AttemptTokens)
	}
}

// TestSettleIsAtomic: when the event cannot be recorded, the reservation
// is kept, so the spend goes on counting until it expires rather than
// vanishing from both tables.
func TestSettleIsAtomic(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	held := reserveTokens(t, ctx, ledger, s, 500)

	bad := costEvent(s, "k", 1, 1)
	bad.InputTokens = -1 // cost_events refuses it by CHECK
	if err := ledger.Settle(ctx, held, bad); err == nil {
		t.Fatal("settled an event the table refuses")
	}
	if sp := spendNow(t, ctx, ledger, s.attempt, s.feature); sp.AttemptTokens != 500 {
		t.Fatalf("attempt spend %d after a failed settle, want the 500 still reserved", sp.AttemptTokens)
	}
}

func TestReleaseDropsReservation(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	held := reserveTokens(t, ctx, ledger, s, 500)
	for range 2 {
		if err := ledger.Release(ctx, held); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	if sp := spendNow(t, ctx, ledger, s.attempt, s.feature); sp.AttemptTokens != 0 {
		t.Fatalf("attempt spend %d after release, want 0", sp.AttemptTokens)
	}
}

func TestSettleRequiresKey(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	if err := ledger.Settle(ctx, reserveTokens(t, ctx, ledger, s, 1), costEvent(s, "", 1, 1)); err == nil {
		t.Fatal("recorded an event with no idempotency key")
	}
}
