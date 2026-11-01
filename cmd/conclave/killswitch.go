package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/alert"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// run is one open workflow execution. ParentID is set for a child, such
// as a TaskWorkflow, whose parent's cancellation reaches it.
type run struct {
	ID, RunID, ParentID string
	Start               time.Time
}

// runs is what the kill switch needs from Temporal.
type runs interface {
	// open lists every running workflow in the namespace.
	open(ctx context.Context) ([]run, error)
	// cancel requests cancellation of one run; a run already closed is
	// not an error.
	cancel(ctx context.Context, id, runID string) error
	// pauseReaper and unpauseReaper pause and unpause the reaper's
	// schedule; a schedule that does not exist yet is not an error.
	pauseReaper(ctx context.Context, note string) error
	unpauseReaper(ctx context.Context, note string) error
}

// killSwitch is halt and resume, over injected state, Temporal, sandbox
// and alert functions so each step can be tested without a server.
type killSwitch struct {
	setHalt     func(ctx context.Context, reason string) (store.HaltState, error)
	clearHalt   func(ctx context.Context, reason string) (store.HaltState, error)
	currentHalt func(ctx context.Context) (store.HaltState, error)
	runs        runs
	// sweep releases and reaps every sandbox with no age cutoff.
	sweep func(ctx context.Context) (activity.SweepResult, error)
	// remaining counts unreleased attempts and labelled containers left.
	remaining func(ctx context.Context) (unreleased, containers int, err error)
	// raise writes an alert to the local log and delivers it.
	raise func(ctx context.Context, a alert.Alert) error
	// connect, when set, is called once the halt is recorded and before
	// any other step. It replaces runs, sweep, remaining and raise with
	// working ones where it can reach their systems, and leaves the
	// failing stand-ins in place where it cannot: Temporal, Docker or
	// GitHub being down is a reason to halt, never a reason the halt is
	// not recorded.
	connect func(ctx context.Context)

	// drain bounds the wait for open workflows to close; poll is the wait
	// between listings.
	drain, poll time.Duration
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	out         io.Writer
}

// unavailableRuns stands in for Temporal when it cannot be reached: every
// call fails with why, so the halt reports the step and goes on.
type unavailableRuns struct{ err error }

func (u unavailableRuns) open(context.Context) ([]run, error)          { return nil, u.err }
func (u unavailableRuns) cancel(context.Context, string, string) error { return u.err }
func (u unavailableRuns) pauseReaper(context.Context, string) error    { return u.err }
func (u unavailableRuns) unpauseReaper(context.Context, string) error  { return u.err }

// unavailable sets every step that needs Temporal, Docker or GitHub to
// fail with err, until connect replaces it with a working one.
func (k *killSwitch) unavailable(err error) {
	k.runs = unavailableRuns{err: fmt.Errorf("temporal unavailable: %w", err)}
	k.sweep = func(context.Context) (activity.SweepResult, error) {
		return activity.SweepResult{}, fmt.Errorf("docker unavailable: %w", err)
	}
	k.remaining = func(context.Context) (int, int, error) { return 0, 0, fmt.Errorf("docker unavailable: %w", err) }
	k.raise = func(context.Context, alert.Alert) error { return fmt.Errorf("alerts: %w", err) }
}

// errNotConnected is the stand-ins' error before connect has run.
var errNotConnected = errors.New("not connected")

// errIncomplete means the halt ran every step but something is still
// open or still exists; the summary says what.
var errIncomplete = errors.New("halt incomplete: see the summary and the runbook")

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (k *killSwitch) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(k.out, format+"\n", args...)
}

// halt runs the kill switch. Only a halt that cannot be recorded stops it
// early: every later step is attempted whatever the one before did, and
// each is safe to repeat, so a second halt finishes what the first could
// not. It returns errIncomplete when a workflow is still open or a
// sandbox is still there afterwards.
func (k *killSwitch) halt(ctx context.Context, reason string) error {
	reason = github.RedactText(reason)
	st, err := k.setHalt(ctx, reason)
	if err != nil {
		return fmt.Errorf("recording the halt: %w", err)
	}
	k.printf("halt %d recorded at %s", st.ID, st.Since.UTC().Format(time.RFC3339))
	if k.connect != nil {
		k.connect(ctx)
	}
	if err := k.raise(ctx, alert.Halt(st.ID, reason, st.Since)); err != nil {
		k.printf("warning: halt alert: %v", err)
	}
	if err := k.runs.pauseReaper(ctx, fmt.Sprintf("halt %d", st.ID)); err != nil {
		k.printf("warning: pausing the reaper: %v", err)
	}
	var res alert.HaltResult
	var errs []error
	res.Cancelled, res.StillOpen, err = k.drainRuns(ctx)
	errs = append(errs, err)
	swept, err := k.sweep(ctx)
	res.Released, res.Reaped = swept.Released, swept.Reaped
	errs = append(errs, err)
	res.Unreleased, res.Containers, err = k.remaining(ctx)
	errs = append(errs, err)
	if err := k.raise(ctx, alert.HaltDone(st.ID, res, k.now())); err != nil {
		k.printf("warning: halt result alert: %v", err)
	}
	k.printf("cancelled %d workflows, %d still open; released %d sandboxes, reaped %d; %d attempts unreleased, %d labelled containers remain",
		res.Cancelled, res.StillOpen, res.Released, res.Reaped, res.Unreleased, res.Containers)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", errIncomplete, err)
	}
	if res.StillOpen > 0 || res.Unreleased > 0 || res.Containers > 0 {
		return errIncomplete
	}
	return nil
}

// drainRuns cancels every open root workflow, lists again, and cancels
// any new one, until two listings in a row find none open or drain has
// passed. A label applied
// mid-halt starts a workflow that the next listing catches. Children are
// left to their parents, whose cancellation reaches them and releases
// their sandboxes; a child whose parent is no longer open is cancelled
// directly.
func (k *killSwitch) drainRuns(ctx context.Context) (cancelled, stillOpen int, err error) {
	deadline := k.now().Add(k.drain)
	done := map[string]bool{}
	empty := 0
	for {
		open, err := k.runs.open(ctx)
		if err != nil {
			return cancelled, 0, fmt.Errorf("listing open workflows: %w", err)
		}
		// Temporal's listing trails a start by a moment, so one empty
		// listing could miss a run started just before it: two in a row,
		// a poll apart, end the drain.
		if empty = emptyStreak(empty, open); empty >= 2 {
			return cancelled, 0, nil
		}
		cancelled += k.cancelRoots(ctx, open, done)
		if !k.now().Before(deadline) {
			return cancelled, len(open), nil
		}
		if err := k.sleep(ctx, k.poll); err != nil {
			return cancelled, len(open), err
		}
	}
}

func emptyStreak(streak int, open []run) int {
	if len(open) > 0 {
		return 0
	}
	return streak + 1
}

// cancelRoots cancels each root of open not already in done, and returns
// how many it cancelled. A failed cancel is reported and tried again on
// the next listing.
func (k *killSwitch) cancelRoots(ctx context.Context, open []run, done map[string]bool) int {
	n := 0
	for _, r := range roots(open) {
		key := r.ID + "/" + r.RunID
		if done[key] {
			continue
		}
		if err := k.runs.cancel(ctx, r.ID, r.RunID); err != nil {
			k.printf("warning: cancelling %s: %v", r.ID, err)
			continue
		}
		done[key] = true
		n++
	}
	return n
}

// roots are the runs with no parent among open.
func roots(open []run) []run {
	ids := make(map[string]bool, len(open))
	for _, r := range open {
		ids[r.ID] = true
	}
	var out []run
	for _, r := range open {
		if r.ParentID == "" || !ids[r.ParentID] {
			out = append(out, r)
		}
	}
	return out
}

// resume clears the halt. Every workflow started after the halt was
// recorded, by a label applied while no worker ran, is listed and, unless
// keep, cancelled first, so the halt does not end with work the owner did
// not see start. Workers are started again by hand.
func (k *killSwitch) resume(ctx context.Context, reason string, keep bool) error {
	st, err := k.currentHalt(ctx)
	if err != nil {
		return fmt.Errorf("reading the halt: %w", err)
	}
	if !st.Halted {
		k.printf("not halted; nothing to resume")
		return nil
	}
	open, err := k.runs.open(ctx)
	if err != nil {
		return fmt.Errorf("listing open workflows: %w", err)
	}
	var late []run
	for _, r := range open {
		if r.Start.After(st.Since) {
			late = append(late, r)
			k.printf("started during the halt: %s (%s)", r.ID, r.Start.UTC().Format(time.RFC3339))
		}
	}
	if !keep {
		k.printf("cancelled %d workflows started during the halt", k.cancelRoots(ctx, late, map[string]bool{}))
	}
	if _, err := k.clearHalt(ctx, github.RedactText(reason)); err != nil {
		return fmt.Errorf("clearing halt %d: %w", st.ID, err)
	}
	if err := k.runs.unpauseReaper(ctx, fmt.Sprintf("resume after halt %d", st.ID)); err != nil {
		k.printf("warning: unpausing the reaper: %v", err)
	}
	k.printf("halt %d cleared; start the workers again", st.ID)
	return nil
}
