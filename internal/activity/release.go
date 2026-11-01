package activity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// ReleaseAttemptSandbox releases the sandbox an attempt recorded and marks
// it released. An attempt that recorded no sandbox, or whose sandbox is
// already marked released, is a no-op. The workflows call it from a
// disconnected context on every exit from an attempt, cancellation
// included (RL-3).
//
// Idempotent retry: Release by ID treats a sandbox that no longer exists
// as released, and MarkSandboxReleased stamps the column once. A worker
// that dies after Docker removed the container but before the column was
// written is retried; the retry's Release finds nothing and returns nil,
// and the column is written then.
func (a *Activities) ReleaseAttemptSandbox(ctx context.Context, attemptID string) error {
	row, err := readAttempt(ctx, a.d.DB, attemptID)
	if err != nil {
		return Temporal(err)
	}
	if row.SandboxID == "" || row.Released {
		return nil
	}
	return Temporal(a.releaseSandbox(ctx, attemptID, row.SandboxID))
}

// ReleaseFeatureSandboxes releases every recorded, unreleased sandbox of
// every attempt of a feature request and returns how many it released.
// The feature workflow runs it from a disconnected context as it closes,
// as a sweep behind each attempt's own release.
//
// Idempotent retry: as ReleaseAttemptSandbox, for each sandbox in turn.
func (a *Activities) ReleaseFeatureSandboxes(ctx context.Context, featureRequestID string) (int, error) {
	if err := checkID("feature request", featureRequestID); err != nil {
		return 0, Temporal(err)
	}
	rows, err := attemptsWhere(ctx, a.d.DB, whereFeatureUnreleased, featureRequestID)
	if err != nil {
		return 0, Temporal(err)
	}
	var errs []error
	for _, r := range rows {
		errs = append(errs, a.releaseSandbox(ctx, r.AttemptID, r.SandboxID))
	}
	return len(rows), Temporal(errors.Join(errs...))
}

// releaseSandbox removes one sandbox and then records it released, in
// that order: a column written first would hide a container that was
// never removed from the reaper.
func (a *Activities) releaseSandbox(ctx context.Context, attemptID, sandboxID string) error {
	if err := a.d.Sandboxes.Release(ctx, sandboxID); err != nil {
		return fmt.Errorf("releasing sandbox %s of attempt %s: %w", sandboxID, attemptID, err)
	}
	if err := store.MarkSandboxReleased(ctx, a.d.DB, attemptID); err != nil {
		return fmt.Errorf("marking sandbox of attempt %s released: %w", attemptID, err)
	}
	return nil
}

// InfoLogger is what the sweep logs through: a *slog.Logger, or the
// activity's Temporal logger.
type InfoLogger interface {
	Info(msg string, keyvals ...any)
}

// SweepResult is what a sweep did.
type SweepResult struct {
	// Released is how many recorded sandboxes were released.
	Released int
	// Reaped is how many unrecorded sandboxes the provider removed by age.
	Reaped int
}

// SweepInput is the reaper's cutoff.
type SweepInput struct {
	// OlderThan is the age past which a recorded attempt's sandbox, or an
	// unrecorded container, is an orphan. It must exceed
	// Deps.LongestSandboxHold.
	OlderThan time.Duration
}

// SweepOrphans is the scheduled reaper (NFR-R3): ReaperWorkflow runs it
// on the conclave-reaper schedule. It refuses a cutoff at or below the
// longest any activity on this worker holds a sandbox, so a schedule
// changed by hand cannot remove a live attempt's sandbox.
//
// Idempotent retry: a sandbox already released is released again as a
// no-op (Release is idempotent), MarkSandboxReleased is write-once, and
// Reap removes only what is still there.
func (a *Activities) SweepOrphans(ctx context.Context, in SweepInput) (SweepResult, error) {
	if longest := a.d.LongestSandboxHold(); in.OlderThan <= longest {
		return SweepResult{}, Temporal(fmt.Errorf("%w: sweep cutoff %s does not exceed the longest sandbox hold, %s",
			errNonRetryable, in.OlderThan, longest))
	}
	res, err := SweepOrphans(ctx, a.d.DB, a.d.Sandboxes, in.OlderThan, activity.GetLogger(ctx))
	return res, Temporal(err)
}

// SweepOrphans is the reaper half of NFR-R3, run at worker startup, by
// the scheduled reaper, and with no cutoff by `conclave halt`: it releases every sandbox an attempt recorded
// and never marked released whose attempt started more than olderThan
// ago, then asks the provider to remove its sandboxes older than that.
// The second step covers the one gap the database cannot: a sandbox
// created by a worker that died before SetAttemptSandbox recorded its ID.
//
// olderThan must exceed the longest an attempt can run (its
// StartToCloseTimeout), or a live attempt's sandbox would be removed
// under it. A failure on one sandbox does not stop the sweep.
func SweepOrphans(ctx context.Context, db store.QueryTxDB, sandboxes contracts.SandboxProvider, olderThan time.Duration, log InfoLogger) (SweepResult, error) {
	orphans, err := store.UnreleasedAttempts(ctx, db, olderThan)
	if err != nil {
		return SweepResult{}, err
	}
	var res SweepResult
	var errs []error
	for _, o := range orphans {
		if err := sandboxes.Release(ctx, o.SandboxID); err != nil {
			errs = append(errs, fmt.Errorf("releasing orphaned sandbox %s of attempt %s: %w", o.SandboxID, o.Attempt.ID, err))
			continue
		}
		if err := store.MarkSandboxReleased(ctx, db, o.Attempt.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		res.Released++
		log.Info("released orphaned sandbox", "attempt_id", o.Attempt.ID, "sandbox_id", o.SandboxID, "attempt_state", string(o.Attempt.State))
	}
	reaped, err := sandboxes.Reap(ctx, olderThan)
	res.Reaped = reaped
	errs = append(errs, err)
	return res, errors.Join(errs...)
}
