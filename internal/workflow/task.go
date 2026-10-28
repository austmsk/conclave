package workflow

import (
	"errors"
	"strings"

	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// TaskWorkflow runs attempts at one task until one decides it: an attempt
// that passes, fails, or hits a platform cap ends the task, and an errored
// attempt gets a new ordinal until MaxErroredAttempts. It makes no GitHub
// call and does no delivery, and its input is serializable, so Milestone
// 2's benchmark starts it directly.
//
// Every attempt's sandbox is released from a disconnected context on
// every exit, cancellation included, here in the child where the sandbox
// ID lives (RL-3). A cancelled task records its open attempt
// errored/cancelled, releases the sandbox, moves the task to cancelled,
// and returns the cancellation.
func TaskWorkflow(ctx workflow.Context, in TaskInput) (TaskResult, error) {
	t := &taskRun{in: in, log: workflow.GetLogger(ctx), actor: store.ActorWorkflow(workflow.GetInfo(ctx).WorkflowExecution.ID)}
	if t.in.Role == "" {
		t.in.Role = contracts.RoleImplementer
	}
	if t.in.Ordinal < 1 {
		t.in.Ordinal = 1
	}
	errored := 0
	for ordinal := t.in.Ordinal; ; ordinal++ {
		res, next, err := t.attempt(ctx, ordinal)
		res.Attempts = ordinal - t.in.Ordinal + 1
		if err != nil || !next {
			return res, err
		}
		errored++
		if errored >= MaxErroredAttempts {
			return t.stop(ctx, TaskResult{Outcome: TaskErrored, AttemptID: res.AttemptID, Reason: res.Reason, Attempts: res.Attempts})
		}
		t.log.Info("attempt errored; starting a new ordinal", "task_id", in.TaskID, "ordinal", ordinal+1, "reason", string(res.Reason))
	}
}

// taskRun is one TaskWorkflow in progress.
type taskRun struct {
	in    TaskInput
	log   log.Logger
	actor string
}

// attempt runs one ordinal. next reports an errored attempt that a new
// ordinal may follow; otherwise res is the task's result.
func (t *taskRun) attempt(ctx workflow.Context, ordinal int) (res TaskResult, next bool, err error) {
	var ref activity.AttemptRef
	err = workflow.ExecuteActivity(smallOptions(ctx), acts.PrepareAttempt, activity.PrepareAttemptInput{
		TaskID: t.in.TaskID, Ordinal: ordinal, Role: t.in.Role,
	}).Get(ctx, &ref)
	if err != nil {
		return t.notRun(ctx, err)
	}
	defer t.release(ctx, ref.AttemptID)
	out := activity.AttemptOutcome{AttemptID: ref.AttemptID, State: ref.State, Reason: ref.Reason, ArtifactHash: ref.ArtifactHash}
	var runErr error
	if ref.State == contracts.AttemptRunning {
		runErr = workflow.ExecuteActivity(attemptOptions(ctx, ref.Timeout), acts.RunAttempt, activity.RunAttemptInput{
			AttemptID: ref.AttemptID, TaskID: t.in.TaskID, BaseSHA: t.in.BaseSHA, Repo: t.in.Repo, Role: t.in.Role,
		}).Get(ctx, &out)
	}
	if ctx.Err() != nil {
		return t.cancelled(ctx, ref.AttemptID)
	}
	if runErr != nil {
		if out, err = t.settle(ctx, ref.AttemptID); err != nil {
			return t.notRun(ctx, err)
		}
	}
	return t.route(ctx, out, runErr)
}

// notRun handles an error outside the attempt activity: a cancellation,
// or a small activity that failed for good. The task stops in
// needs_attention with reason error.
func (t *taskRun) notRun(ctx workflow.Context, err error) (TaskResult, bool, error) {
	if ctx.Err() != nil {
		return t.cancelled(ctx, "")
	}
	t.log.Error("task step failed", "task_id", t.in.TaskID, "error", err)
	res, err := t.stop(ctx, TaskResult{Outcome: TaskErrored, Reason: contracts.ReasonSandboxError, Detail: errorDetail(err)})
	return res, false, err
}

// settle finishes an attempt whose activity returned an error, and reads
// back how Postgres recorded it: the attempt may have recorded its outcome
// and then lost its result, and the record is what the task routes on.
func (t *taskRun) settle(ctx workflow.Context, attemptID string) (activity.AttemptOutcome, error) {
	var out activity.AttemptOutcome
	err := workflow.ExecuteActivity(smallOptions(ctx), acts.SettleAttempt, activity.SettleInput{
		AttemptID: attemptID, Reason: contracts.ReasonSandboxError, Actor: t.actor,
	}).Get(ctx, &out)
	return out, err
}

// route decides the task from an attempt's recorded outcome and the error
// its activity returned, if any. The outcome table is in
// docs/tickets/ticket-8-pre-plan.md.
func (t *taskRun) route(ctx workflow.Context, out activity.AttemptOutcome, runErr error) (TaskResult, bool, error) {
	res := TaskResult{AttemptID: out.AttemptID, ArtifactHash: out.ArtifactHash, Reason: out.Reason}
	switch {
	case out.State == contracts.AttemptPassed:
		res.Outcome = TaskPassed
		return t.finish(ctx, res, contracts.TaskDone)
	case out.State == contracts.AttemptFailed:
		res.Outcome = TaskFailed
		return t.finish(ctx, res, contracts.TaskNeedsAttention)
	case out.Reason == contracts.ReasonBudgetCap:
		res.Outcome, res.BudgetKind = TaskBudgetCap, budgetKind(runErr)
		return t.finish(ctx, res, contracts.TaskNeedsAttention)
	case isPermanent(runErr):
		res.Outcome, res.Detail = TaskErrored, errorDetail(runErr)
		return t.finish(ctx, res, contracts.TaskNeedsAttention)
	}
	return res, true, nil
}

// finish moves the task to its final status for this run and returns res.
func (t *taskRun) finish(ctx workflow.Context, res TaskResult, to contracts.TaskStatus) (TaskResult, bool, error) {
	if err := t.moveTask(ctx, to, string(res.Reason)); err != nil {
		return t.notRun(ctx, err)
	}
	return res, false, nil
}

// stop moves the task to needs_attention and returns res.
func (t *taskRun) stop(ctx workflow.Context, res TaskResult) (TaskResult, error) {
	if err := t.moveTask(ctx, contracts.TaskNeedsAttention, string(res.Reason)); err != nil {
		if ctx.Err() != nil {
			res, _, err = t.cancelled(ctx, "")
			return res, err
		}
		t.log.Error("moving task to needs_attention", "task_id", t.in.TaskID, "error", err)
	}
	return res, nil
}

// moveTask reads the task's state and revision and makes the change from
// there. A task already in to is left alone.
func (t *taskRun) moveTask(ctx workflow.Context, to contracts.TaskStatus, reason string) error {
	actx := smallOptions(ctx)
	var ref activity.TaskRef
	if err := workflow.ExecuteActivity(actx, acts.LoadTask, t.in.TaskID).Get(ctx, &ref); err != nil {
		return err
	}
	if ref.Status == to {
		return nil
	}
	return workflow.ExecuteActivity(actx, acts.Transition, activity.TransitionInput{
		Kind: contracts.EntityTask, ID: t.in.TaskID, From: string(ref.Status), To: string(to),
		Revision: ref.Revision, Reason: reason, Actor: t.actor,
	}).Get(ctx, nil)
}

// cancelled closes the task after a cancellation, on a context that
// survives it: the open attempt is recorded errored/cancelled, its
// sandbox released, and the task moved to cancelled. The deferred release
// of the attempt runs after this as well.
func (t *taskRun) cancelled(ctx workflow.Context, attemptID string) (TaskResult, bool, error) {
	cctx := cleanupContext(ctx)
	if attemptID != "" {
		if err := workflow.ExecuteActivity(cctx, acts.SettleAttempt, activity.SettleInput{
			AttemptID: attemptID, Reason: contracts.ReasonCancelled, Actor: t.actor,
		}).Get(cctx, nil); err != nil {
			t.log.Error("settling cancelled attempt", "attempt_id", attemptID, "error", err)
		}
	}
	if err := workflow.ExecuteActivity(cctx, acts.CancelTask, activity.CancelTaskInput{TaskID: t.in.TaskID, Actor: t.actor}).Get(cctx, nil); err != nil {
		t.log.Error("cancelling task", "task_id", t.in.TaskID, "error", err)
	}
	return TaskResult{Outcome: TaskCancelled, AttemptID: attemptID, Reason: contracts.ReasonCancelled}, false, ctx.Err()
}

// release releases an attempt's sandbox from a disconnected context. It
// is deferred for every attempt, so it runs on every exit path; the
// activity is a no-op when nothing was recorded or it is already released.
func (t *taskRun) release(ctx workflow.Context, attemptID string) {
	cctx := cleanupContext(ctx)
	if err := workflow.ExecuteActivity(cctx, acts.ReleaseAttemptSandbox, attemptID).Get(cctx, nil); err != nil {
		t.log.Error("releasing attempt sandbox; the startup sweep will retry", "attempt_id", attemptID, "error", err)
	}
}

// budgetKind is the cap named by a budget_cap:<kind> error type. An
// attempt that recorded budget_cap and then lost its result has no error
// to read; its kind is unknown.
func budgetKind(err error) llm.BudgetKind {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		if kind, ok := activity.BudgetKindOf(app.Type()); ok {
			return kind
		}
	}
	return ""
}

// isPermanent reports whether an activity error is one no new attempt can
// fix: a non-retryable ApplicationError other than a cap refusal.
func isPermanent(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.NonRetryable()
}

// maxDetail bounds the error text a reason carries.
const maxDetail = 200

// errorDetail is an error's text, bounded, for a reason's detail. Every
// activity error is written to be safe to persist.
func errorDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		msg = app.Message()
	}
	if len(msg) > maxDetail {
		msg = strings.ToValidUTF8(msg[:maxDetail], "") + "..."
	}
	return msg
}
