package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
)

// TestCancelDuringAgentPhase sends /cancel while the attempt is running:
// the attempt records errored/cancelled, the task and the feature request
// reach cancelled, and the sandbox is released.
func TestCancelDuringAgentPhase(t *testing.T) {
	w := newWorld()
	env := newEnv(t, w)
	w.run = blocksUntilCancelled(func() {
		env.SignalWorkflow(SignalName(github.CommandCancel), command(github.CommandCancel, 300))
	})
	signalAfter(env, time.Minute, command(github.CommandApprove, 100))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "attempts", w.attemptStates(), []string{"errored/cancelled"})
	equal(t, "task", w.taskStatus(), contracts.TaskCancelled)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, "executing>cancelled"})
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
	if w.called("ReleaseAttemptSandbox") == 0 {
		t.Error("the attempt's own release never ran")
	}
}

// TestCancelParentReleasesChildSandbox cancels the feature workflow
// itself, through Temporal, mid-attempt: the cancellation reaches the
// child, the child still releases its sandbox from a disconnected
// context, and the feature request is closed as cancelled.
func TestCancelParentReleasesChildSandbox(t *testing.T) {
	w := newWorld()
	env := newEnv(t, w)
	w.run = blocksUntilCancelled(env.CancelWorkflow)
	signalAfter(env, time.Minute, command(github.CommandApprove, 100))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	var canceled *temporal.CanceledError
	if err := env.GetWorkflowError(); !errors.As(err, &canceled) {
		t.Fatalf("workflow error = %v, want a cancellation", err)
	}
	equalSlices(t, "attempts", w.attemptStates(), []string{"errored/cancelled"})
	equal(t, "task", w.taskStatus(), contracts.TaskCancelled)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, "executing>cancelled"})
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
	if w.called("ReleaseAttemptSandbox") == 0 {
		t.Error("the child's deferred release never ran")
	}
}

// TestCancelTaskWorkflowDirectly cancels a task workflow started on its
// own, as Milestone 2's benchmark starts it: the release still runs.
func TestCancelTaskWorkflowDirectly(t *testing.T) {
	w := newWorld()
	w.task = contracts.TaskRunning
	env := newEnv(t, w)
	w.run = blocksUntilCancelled(env.CancelWorkflow)
	env.ExecuteWorkflow(TaskWorkflow, TaskInput{TaskID: taskID, Ordinal: 1, BaseSHA: baseSHA, RepoFullName: featureInput().Start.Repo.FullName, Role: contracts.RoleImplementer})
	equalSlices(t, "attempts", w.attemptStates(), []string{"errored/cancelled"})
	equal(t, "task", w.taskStatus(), contracts.TaskCancelled)
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
}

// TestTaskWorkflowDirectly runs a task workflow on its own to a pass,
// with no GitHub activity called.
func TestTaskWorkflowDirectly(t *testing.T) {
	w := newWorld()
	w.task = contracts.TaskRunning
	w.run = returns(contracts.AttemptPassed, "")
	env := newEnv(t, w)
	env.ExecuteWorkflow(TaskWorkflow, TaskInput{TaskID: taskID, Ordinal: 1, BaseSHA: baseSHA, RepoFullName: featureInput().Start.Repo.FullName, Role: contracts.RoleImplementer})
	checkCompleted(t, env)
	var res TaskResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	equal(t, "outcome", res.Outcome, TaskPassed)
	equal(t, "artifact", res.ArtifactHash, artifact)
	equal(t, "task", w.taskStatus(), contracts.TaskDone)
	for _, name := range []string{"Analyze", "PostPlanComment", "Approve", "Deliver"} {
		equal(t, "calls to "+name, w.called(name), 0)
	}
}

// TestCancelInAwaitingApproval cancels before approval: no attempt, no
// task, and the feature request is cancelled.
func TestCancelInAwaitingApproval(t *testing.T) {
	w := newWorld()
	env := newEnv(t, w)
	signalAfter(env, time.Hour, command(github.CommandCancel, 5))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "moves", w.moves(), []string{toApproval, "awaiting_approval>cancelled"})
	equal(t, "attempts", len(w.attemptStates()), 0)
	equal(t, "approvals", w.approved, 0)
}

// TestCancelInNeedsAttentionReleasesSandbox reaches needs_attention with
// a sandbox the attempt's own release could not remove, then /cancel:
// the feature request is cancelled and the sandbox released.
func TestCancelInNeedsAttentionReleasesSandbox(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptFailed, contracts.ReasonGateFailed)
	w.fail["ReleaseAttemptSandbox"] = temporal.NewNonRetryableApplicationError("docker is down", activity.ErrTypePermanent, nil)
	env := newEnv(t, w)
	var held []string
	env.RegisterDelayedCallback(func() { held = w.unreleased() }, 12*time.Hour)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "sandboxes held in needs_attention", held, []string{"sandbox-1"})
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled})
	equal(t, "task", w.taskStatus(), contracts.TaskCancelled)
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
}

// TestApproveAfterLongWait waits thirty days for /approve, skipped by the
// test suite's clock, and resumes into execution with nothing lost.
func TestApproveAfterLongWait(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	env := newEnv(t, w)
	start := env.Now()
	var approvedAt time.Time
	env.RegisterDelayedCallback(func() {
		equal(t, "status while waiting", queryStatus(t, env).Status, contracts.FeatureAwaitingApproval)
		approvedAt = env.Now()
		env.SignalWorkflow(SignalName(github.CommandApprove), command(github.CommandApprove, 9))
	}, 30*24*time.Hour)
	signalAfter(env, 31*24*time.Hour, command(github.CommandCancel, 10))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	if approvedAt.Sub(start) < 30*24*time.Hour {
		t.Errorf("approved after %v, want at least 30 days", approvedAt.Sub(start))
	}
	equal(t, "approvals", w.approved, 1)
	equal(t, "deliveries", w.delivers, 1)
	equal(t, "plan comments", w.comments, 1)
}

// TestAttemptResultLostAfterRecording has the attempt record passed and
// then lose its result: the workflow settles from Postgres, which holds
// passed, so the task is done and nothing is rerun.
func TestAttemptResultLostAfterRecording(t *testing.T) {
	w := newWorld()
	w.run = func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		w.finish(n, contracts.AttemptPassed, "")
		return activity.AttemptOutcome{}, errors.New("worker lost after recording")
	}
	env := newEnv(t, w)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "attempts", w.attemptStates(), []string{"passed/"})
	equal(t, "attempt runs", w.called("RunAttempt"), 1)
	equal(t, "deliveries", w.delivers, 1)
	equal(t, "task", w.taskStatus(), contracts.TaskDone)
}

// TestAttemptWorkerDied has the attempt die before recording anything:
// SettleAttempt records it errored, and a new ordinal runs.
func TestAttemptWorkerDied(t *testing.T) {
	w := newWorld()
	w.run = func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		if n == 1 {
			return activity.AttemptOutcome{}, errors.New("heartbeat timeout")
		}
		return w.finish(n, contracts.AttemptPassed, ""), nil
	}
	env := newEnv(t, w)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "attempts", w.attemptStates(), []string{"errored/sandbox_error", "passed/"})
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
}

// TestAttemptNotRetried checks the attempt activity runs once even when
// it fails with a retryable error: Temporal must never restart it.
func TestAttemptNotRetried(t *testing.T) {
	w := newWorld()
	w.run = func(context.Context, *world, int) (activity.AttemptOutcome, error) {
		return activity.AttemptOutcome{}, errors.New("transient")
	}
	env := newEnv(t, w)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "attempt runs", w.called("RunAttempt"), MaxErroredAttempts)
	equal(t, "prepared ordinals", w.called("PrepareAttempt"), MaxErroredAttempts)
}

// TestPermanentAttemptErrorStartsNoOrdinal has the attempt record
// errored and return a permanent error: no new ordinal is worth starting.
func TestPermanentAttemptErrorStartsNoOrdinal(t *testing.T) {
	w := newWorld()
	w.run = func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		w.finish(n, contracts.AttemptErrored, contracts.ReasonProviderError)
		return activity.AttemptOutcome{}, temporal.NewNonRetryableApplicationError("role denied", activity.ErrTypePermanent, nil)
	}
	env := newEnv(t, w)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "attempt runs", w.called("RunAttempt"), 1)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, "executing>needs_attention error:role denied", toCancelled})
}

// TestCancelMoveRefusedWaitsForAnotherCancel has the move to cancelled
// fail for good: the workflow neither ends nor reports a cancellation
// Postgres does not hold, and a second /cancel closes the request.
func TestCancelMoveRefusedWaitsForAnotherCancel(t *testing.T) {
	w := newWorld()
	w.refuseMovesTo[contracts.FeatureCancelled] = 1
	env := newEnv(t, w)
	signalAfter(env, time.Hour, command(github.CommandCancel, 80))
	var between Status
	env.RegisterDelayedCallback(func() { between = queryStatus(t, env) }, 2*time.Hour)
	signalAfter(env, 3*time.Hour, command(github.CommandCancel, 81))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "status between the two cancels", between.Status, contracts.FeatureAwaitingApproval)
	equalSlices(t, "moves", w.moves(), []string{toApproval, "awaiting_approval>cancelled"})
}

// TestCancelTaskRefusedFailsTaskWorkflow cancels a task workflow whose
// task cannot be recorded cancelled: the workflow fails with that error
// instead of reporting a cancellation, and the sandbox is still released.
func TestCancelTaskRefusedFailsTaskWorkflow(t *testing.T) {
	w := newWorld()
	w.task = contracts.TaskRunning
	w.fail["CancelTask"] = temporal.NewNonRetryableApplicationError("refused", activity.ErrTypePermanent, nil)
	env := newEnv(t, w)
	w.run = blocksUntilCancelled(env.CancelWorkflow)
	env.ExecuteWorkflow(TaskWorkflow, TaskInput{TaskID: taskID, Ordinal: 1, BaseSHA: baseSHA, RepoFullName: featureInput().Start.Repo.FullName, Role: contracts.RoleImplementer})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	var canceled *temporal.CanceledError
	if err := env.GetWorkflowError(); err == nil || errors.As(err, &canceled) {
		t.Fatalf("workflow error = %v, want the CancelTask failure, not a cancellation", err)
	}
	equal(t, "task", w.taskStatus(), contracts.TaskRunning)
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
}
