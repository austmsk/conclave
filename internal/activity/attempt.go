package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// recordTimeout bounds each write that must land even after the
// activity's context is cancelled: recording an outcome, releasing a
// sandbox.
const recordTimeout = 2 * time.Minute

// AgentOutputCap is the most bytes of one tool result the model is shown;
// the full output goes to the blob store.
const AgentOutputCap = 16 << 10

// PrepareAttempt records an attempt at a task, in state running, with the
// provenance the router is configured to use for the role, and returns it
// with the StartToCloseTimeout RunAttempt needs.
//
// Idempotent retry: store.CreateAttempt is keyed on (task, ordinal), so a
// retry returns the row the first call made, in whatever state it has
// reached. There is no outside action.
func (a *Activities) PrepareAttempt(ctx context.Context, in PrepareAttemptInput) (AttemptRef, error) {
	ref, err := a.prepareAttempt(ctx, in)
	return ref, Temporal(err)
}

func (a *Activities) prepareAttempt(ctx context.Context, in PrepareAttemptInput) (AttemptRef, error) {
	pc, err := readPlanContext(ctx, a.d.DB, in.TaskID)
	if err != nil {
		return AttemptRef{}, err
	}
	if pc.TaskStatus != contracts.TaskRunning {
		return AttemptRef{}, fmt.Errorf("%w: task %s is %s, not running", errNonRetryable, in.TaskID, pc.TaskStatus)
	}
	cfg, err := a.repo(pc.RepoFullName)
	if err != nil {
		return AttemptRef{}, err
	}
	spec, err := a.d.roleSpec(in.Role)
	if err != nil {
		return AttemptRef{}, err
	}
	prov, err := a.d.provenance(in.Role)
	if err != nil {
		return AttemptRef{}, err
	}
	at, _, err := store.CreateAttempt(ctx, a.d.DB, store.AttemptParams{TaskID: in.TaskID, Ordinal: in.Ordinal, Provenance: prov})
	if err != nil {
		return AttemptRef{}, err
	}
	return AttemptRef{
		AttemptID: at.ID, Ordinal: at.Ordinal, Revision: at.Revision, State: at.State, Reason: at.Reason,
		ArtifactHash: at.ArtifactHash, Timeout: AttemptTimeout(spec, cfg.Sandbox.CommandTimeout),
	}, nil
}

// RunAttempt is one attempt, end to end, in one activity on one worker:
// clone the base commit, create the sandbox (with the install phase when
// one is configured), record its ID on the attempt, run the agent,
// freeze the artifact and record its hash, gate it, record the outcome,
// and release the sandbox. The runner owns the loop and the gate needs
// the live sandbox and the clone, so the attempt cannot be split
// (decision 0012).
//
// The workflow runs it with MaximumAttempts 1: an attempt cannot restart,
// and artifact_hash is write-once. It is still safe to run twice: an
// attempt already past running returns its recorded outcome and runs
// nothing.
//
// Crash gaps, outside action first:
//   - sandbox created, ID not yet recorded: SetAttemptSandbox is the
//     first write after Create; if it fails the sandbox is released at
//     once. If the worker dies in between, the provider's Reap by age,
//     run by SweepOrphans at worker startup, removes it.
//   - ID recorded, sandbox not released: the workflow's disconnected
//     ReleaseAttemptSandbox, and the startup sweep, release it by the
//     recorded ID.
//   - outcome recorded, result lost: SettleAttempt reads the recorded
//     outcome back, so the workflow routes on what Postgres holds.
//
// Recorded outcomes return as values. A feature request or monthly cap
// returns, after recording, a non-retryable error of type
// budget_cap:<kind>; a permanent failure, after recording, one of type
// permanent; cancellation, after recording errored/cancelled, the
// context's error.
func (a *Activities) RunAttempt(ctx context.Context, in RunAttemptInput) (AttemptOutcome, error) {
	stop := startHeartbeat(ctx, a.d.HeartbeatEvery, recordHeartbeat)
	defer stop()
	r, recorded, err := a.newAttemptRun(ctx, in)
	if err != nil {
		return AttemptOutcome{}, Temporal(err)
	}
	if recorded != nil {
		return *recorded, nil
	}
	defer r.removeScratch()
	out, err := r.execute(ctx)
	return out, Temporal(err)
}

// newAttemptRun loads and checks everything the attempt needs before any
// outside action. An attempt no longer running returns its recorded
// outcome instead.
func (a *Activities) newAttemptRun(ctx context.Context, in RunAttemptInput) (*attemptRun, *AttemptOutcome, error) {
	row, err := readAttempt(ctx, a.d.DB, in.AttemptID)
	if err != nil {
		return nil, nil, err
	}
	if row.TaskID != in.TaskID {
		return nil, nil, fmt.Errorf("%w: attempt %s belongs to task %s, not %s", errNonRetryable, in.AttemptID, row.TaskID, in.TaskID)
	}
	if row.State != contracts.AttemptRunning {
		out := row.outcome()
		return nil, &out, nil
	}
	pc, err := readPlanContext(ctx, a.d.DB, in.TaskID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkRunContext(in, pc); err != nil {
		return nil, nil, err
	}
	r := &attemptRun{a: a, in: in, row: row, pc: pc, info: activity.GetInfo(ctx), log: activity.GetLogger(ctx)}
	if r.cfg, err = a.repo(pc.RepoFullName); err != nil {
		return nil, nil, err
	}
	if r.spec, err = a.d.roleSpec(in.Role); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(pc.Body, &r.body); err != nil {
		return nil, nil, fmt.Errorf("%w: decoding body of plan %s: %v", errNonRetryable, pc.PlanID, err)
	}
	return r, nil, nil
}

// checkRunContext refuses an attempt whose input disagrees with the
// records: it must run against the plan's pinned base commit, on the
// plan's repository, under an approved plan and a running task.
func checkRunContext(in RunAttemptInput, pc planContext) error {
	switch {
	case pc.PlanStatus != contracts.PlanApproved:
		return fmt.Errorf("%w: plan %s is %s, not approved", errNonRetryable, pc.PlanID, pc.PlanStatus)
	case pc.BaseSHA == "" || pc.BaseSHA != in.BaseSHA:
		return fmt.Errorf("%w: base %q is not the commit plan %s pinned (%q)", errNonRetryable, in.BaseSHA, pc.PlanID, pc.BaseSHA)
	case pc.RepoFullName != in.RepoFullName:
		return fmt.Errorf("%w: task %s is in %s, not %s", errNonRetryable, in.TaskID, pc.RepoFullName, in.RepoFullName)
	case pc.TaskStatus != contracts.TaskRunning:
		return fmt.Errorf("%w: task %s is %s, not running", errNonRetryable, in.TaskID, pc.TaskStatus)
	}
	return nil
}

// removeScratch deletes the clone.
func (r *attemptRun) removeScratch() {
	if r.scratch == "" {
		return
	}
	if err := os.RemoveAll(r.scratch); err != nil {
		r.log.Warn("removing attempt scratch directory", "attempt_id", r.in.AttemptID, "error", err)
	}
}

// detached returns a context that survives ctx's cancellation, bounded by
// d, for writes that must land after an attempt is cancelled.
func detached(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}
