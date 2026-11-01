package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// haltLockKey serializes halt and resume, so two `conclave halt` runs
// started together record one halt. It is "halt" in ASCII, distinct from
// budgetLockKey.
const haltLockKey int64 = 0x68616c74

// Halt event kinds, as halt_events stores them.
const (
	haltSet   = "set"
	haltClear = "clear"
)

// ActorCLI attributes a change made by the owner through cmd/conclave,
// such as a halt or a resume.
const ActorCLI = "cli"

// HaltState is the kill switch's current state: the latest halt event.
// When Halted, ID and Since name the set event; when not, they name the
// latest clear, or are zero when nothing was ever recorded.
type HaltState struct {
	Halted bool
	ID     int64
	Since  time.Time
	Reason string
}

// ErrEmptyReason means a halt or resume was recorded without a reason.
var ErrEmptyReason = errors.New("halt event needs a reason")

// CurrentHalt returns the kill switch's state from the latest event.
func CurrentHalt(ctx context.Context, db DBTX) (HaltState, error) {
	var (
		st   HaltState
		kind string
	)
	err := db.QueryRow(ctx, `SELECT id, kind, occurred_at, reason FROM halt_events ORDER BY id DESC LIMIT 1`).
		Scan(&st.ID, &kind, &st.Since, &st.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return HaltState{}, nil
	}
	if err != nil {
		return HaltState{}, fmt.Errorf("reading halt state: %w", err)
	}
	st.Halted = kind == haltSet
	return st, nil
}

// SetHalt records a halt and returns the state after it. While already
// halted it records nothing and returns the existing halt, so running
// `conclave halt` twice keeps the first halt's time, which resume uses
// to find runs started during it.
func SetHalt(ctx context.Context, db TxDB, reason, actor string) (HaltState, error) {
	return recordHalt(ctx, db, haltSet, reason, actor)
}

// ClearHalt records a resume and returns the state after it. While not
// halted it records nothing.
func ClearHalt(ctx context.Context, db TxDB, reason, actor string) (HaltState, error) {
	return recordHalt(ctx, db, haltClear, reason, actor)
}

func recordHalt(ctx context.Context, db TxDB, kind, reason, actor string) (HaltState, error) {
	if reason == "" || actor == "" {
		return HaltState{}, fmt.Errorf("recording halt %s: %w", kind, ErrEmptyReason)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return HaltState{}, fmt.Errorf("recording halt %s: %w", kind, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, haltLockKey); err != nil {
		return HaltState{}, fmt.Errorf("locking halt state: %w", err)
	}
	cur, err := CurrentHalt(ctx, tx)
	if err != nil {
		return HaltState{}, err
	}
	// Already in the state asked for: a set while halted, or a clear
	// while running (including before any event).
	if cur.Halted == (kind == haltSet) {
		return cur, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO halt_events (kind, reason, actor) VALUES ($1, $2, $3)`,
		kind, reason, actor); err != nil {
		return HaltState{}, fmt.Errorf("recording halt %s: %w", kind, err)
	}
	st, err := CurrentHalt(ctx, tx)
	if err != nil {
		return HaltState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HaltState{}, fmt.Errorf("committing halt %s: %w", kind, err)
	}
	return st, nil
}
