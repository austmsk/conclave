package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// SettleAttempt ends an attempt the attempt activity did not report: it
// timed out, its worker died, it was cancelled, or it returned an error.
// An attempt still running is finished errored with the given reason; an
// attempt that already reached a terminal state is left as it is, and its
// recorded outcome is returned. Postgres, not the error the workflow
// received, says how the attempt ended: an attempt activity that finished
// the row and then lost its result is reported as it was recorded.
//
// Idempotent retry: the finish is a compare-and-set from running, so a
// second call returns the outcome the first recorded. There is no
// outside action; the sandbox is released separately.
func (a *Activities) SettleAttempt(ctx context.Context, in SettleInput) (AttemptOutcome, error) {
	out, err := a.settle(ctx, in)
	return out, Temporal(err)
}

func (a *Activities) settle(ctx context.Context, in SettleInput) (AttemptOutcome, error) {
	row, err := readAttempt(ctx, a.d.DB, in.AttemptID)
	if err != nil {
		return AttemptOutcome{}, err
	}
	if row.State != contracts.AttemptRunning {
		return row.outcome(), nil
	}
	return a.finishErrored(ctx, row, in.Reason, in.Actor)
}

// finishErrored finishes a running attempt errored with reason. Only
// FinishAttempt writes an attempt's usage, so a row finished here keeps
// zero usage and no report; what the attempt spent is still in
// cost_events, which the router wrote per call. If the attempt left
// running meanwhile, the recorded outcome is returned instead.
func (a *Activities) finishErrored(ctx context.Context, row attemptRow, reason contracts.FailureReason, actor string) (AttemptOutcome, error) {
	err := pgx.BeginFunc(ctx, a.d.DB, func(tx pgx.Tx) error {
		return applied(store.FinishAttempt(ctx, tx, store.FinishParams{
			AttemptID: row.AttemptID, Revision: row.Revision, To: contracts.AttemptErrored, Reason: reason, Actor: actor,
		}))
	})
	var wrong *store.WrongStateError
	if err != nil && !errors.As(err, &wrong) {
		return AttemptOutcome{}, fmt.Errorf("settling attempt %s: %w", row.AttemptID, err)
	}
	now, err := readAttempt(ctx, a.d.DB, row.AttemptID)
	if err != nil {
		return AttemptOutcome{}, err
	}
	return now.outcome(), nil
}
