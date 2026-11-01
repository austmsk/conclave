package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/store"
)

// TestHaltStateLatestEventWins walks the kill switch through clear, set,
// clear and set again as the application role: the state is always the
// latest event, a clear before any halt records nothing, and each
// recorded event is later than the one before.
func TestHaltStateLatestEventWins(t *testing.T) {
	ctx, tx := testTx(t)
	steps := []struct {
		name       string
		set        bool
		wantHalted bool
		wantNew    bool
	}{
		{"clear before any halt", false, false, false},
		{"set", true, true, true},
		{"clear", false, false, true},
		{"set again", true, true, true},
	}
	err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
		var last int64
		for _, step := range steps {
			record := store.ClearHalt
			if step.set {
				record = store.SetHalt
			}
			st, err := record(ctx, sp, step.name, store.ActorCLI)
			if err != nil {
				return err
			}
			if st.Halted != step.wantHalted || (st.ID > last) != step.wantNew {
				t.Fatalf("%s: %+v after event %d; want halted %t, new event %t", step.name, st, last, step.wantHalted, step.wantNew)
			}
			last = st.ID
			if cur, err := store.CurrentHalt(ctx, sp); err != nil || cur != st {
				t.Fatalf("%s: CurrentHalt = %+v, %v; want %+v", step.name, cur, err, st)
			}
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
