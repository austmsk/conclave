package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/store"
)

// TestHaltStateLatestEventWins walks the kill switch through set, clear
// and set again as the application role: the state is always the latest
// event, and a repeated set or clear records nothing.
func TestHaltStateLatestEventWins(t *testing.T) {
	ctx, tx := testTx(t)
	err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
		st, err := store.CurrentHalt(ctx, sp)
		if err != nil || st.Halted || st.ID != 0 {
			t.Fatalf("before any event: %+v, %v; want not halted, no ID", st, err)
		}
		if st, err := store.ClearHalt(ctx, sp, "nothing to clear", store.ActorCLI); err != nil || st.Halted || st.ID != 0 {
			t.Fatalf("clear before any halt: %+v, %v; want nothing recorded", st, err)
		}
		first, err := store.SetHalt(ctx, sp, "first", store.ActorCLI)
		if err != nil || !first.Halted || first.Reason != "first" {
			t.Fatalf("set: %+v, %v", first, err)
		}
		cleared, err := store.ClearHalt(ctx, sp, "resume", store.ActorCLI)
		if err != nil || cleared.Halted || cleared.ID <= first.ID {
			t.Fatalf("clear: %+v, %v; want a later, cleared event", cleared, err)
		}
		again, err := store.SetHalt(ctx, sp, "again", store.ActorCLI)
		if err != nil || !again.Halted || again.ID <= cleared.ID {
			t.Fatalf("second set: %+v, %v; want a later halt", again, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSetHaltTwiceStaysOne halts twice: the second keeps the first halt's
// ID and time, which resume uses to find runs started during the halt.
func TestSetHaltTwiceStaysOne(t *testing.T) {
	ctx, tx := testTx(t)
	err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
		a, err := store.SetHalt(ctx, sp, "first", store.ActorCLI)
		if err != nil {
			return err
		}
		b, err := store.SetHalt(ctx, sp, "second", store.ActorCLI)
		if err != nil {
			return err
		}
		if a != b {
			t.Fatalf("second halt = %+v, want the first, %+v", b, a)
		}
		var n int
		if err := sp.QueryRow(ctx, `SELECT count(*) FROM halt_events`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("%d halt events, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHaltNeedsReason refuses a halt or resume with no reason or actor.
func TestHaltNeedsReason(t *testing.T) {
	ctx, tx := testTx(t)
	if _, err := store.SetHalt(ctx, tx, "", store.ActorCLI); !errors.Is(err, store.ErrEmptyReason) {
		t.Fatalf("set without reason: %v, want ErrEmptyReason", err)
	}
	if _, err := store.ClearHalt(ctx, tx, "x", ""); !errors.Is(err, store.ErrEmptyReason) {
		t.Fatalf("clear without actor: %v, want ErrEmptyReason", err)
	}
}
