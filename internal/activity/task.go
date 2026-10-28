package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// CreateTask records the approved plan's task, in state running.
//
// Idempotent retry: store.CreateTask is keyed on (plan, task key), so a
// retry returns the row the first call made. The task spec comes from the
// plan body, never from the issue. There is no outside action.
func (a *Activities) CreateTask(ctx context.Context, planID string) (TaskRef, error) {
	ref, err := a.createTask(ctx, planID)
	return ref, Temporal(err)
}

func (a *Activities) createTask(ctx context.Context, planID string) (TaskRef, error) {
	plan, body, err := a.planOf(ctx, planID)
	if err != nil {
		return TaskRef{}, err
	}
	if plan.Status != contracts.PlanApproved {
		return TaskRef{}, fmt.Errorf("%w: plan %s is %s, not approved", errNonRetryable, planID, plan.Status)
	}
	t, _, err := store.CreateTask(ctx, a.d.DB, planID, body.Task)
	if err != nil {
		return TaskRef{}, err
	}
	return TaskRef{TaskID: t.ID, Key: t.Spec.Key, Status: t.Status, Revision: t.Revision}, nil
}

// LoadTask reads a task's state and revision. It is a read, safe to
// repeat.
func (a *Activities) LoadTask(ctx context.Context, taskID string) (TaskRef, error) {
	ref, err := readTask(ctx, a.d.DB, taskID)
	return ref, Temporal(err)
}

// CancelTask closes everything still open under a cancelled task: every
// attempt still running is finished errored with reason cancelled,
// every recorded sandbox not yet released is released, and the task
// moves to cancelled. It covers an attempt the workflow never heard
// about, such as one whose create committed while the activity that made
// it was being cancelled.
//
// Idempotent retry: each step only acts on what is still open, so a
// second run finds nothing to finish, Release of a removed sandbox is a
// no-op, and a task already cancelled is left alone. A task that is
// already done or cancelled is not moved.
func (a *Activities) CancelTask(ctx context.Context, in CancelTaskInput) error {
	return Temporal(a.cancelTask(ctx, in))
}

func (a *Activities) cancelTask(ctx context.Context, in CancelTaskInput) error {
	if err := checkID("task", in.TaskID); err != nil {
		return err
	}
	attempts, err := attemptsWhere(ctx, a.d.DB, whereTask, in.TaskID)
	if err != nil {
		return err
	}
	var errs []error
	for _, at := range attempts {
		if at.State == contracts.AttemptRunning {
			_, err := a.finishErrored(ctx, at, contracts.ReasonCancelled, in.Actor)
			errs = append(errs, err)
		}
		if at.SandboxID != "" && !at.Released {
			errs = append(errs, a.releaseSandbox(ctx, at.AttemptID, at.SandboxID))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("closing attempts of task %s: %w", in.TaskID, err)
	}
	return a.cancelTaskRow(ctx, in)
}

// cancelTaskRow moves the task to cancelled from running or
// needs_attention, at the revision it holds now.
func (a *Activities) cancelTaskRow(ctx context.Context, in CancelTaskInput) error {
	t, err := readTask(ctx, a.d.DB, in.TaskID)
	if err != nil {
		return err
	}
	if !t.Status.CanMoveTo(contracts.TaskCancelled) {
		return nil
	}
	return pgx.BeginFunc(ctx, a.d.DB, func(tx pgx.Tx) error {
		return applied(store.Transition(ctx, tx, store.TransitionRequest{
			Kind: contracts.EntityTask, ID: in.TaskID, Revision: t.Revision,
			From: string(t.Status), To: string(contracts.TaskCancelled), Actor: in.Actor,
		}))
	})
}
