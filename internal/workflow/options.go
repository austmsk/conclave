package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
)

// Small activities are database writes, comments and the release. They
// use Temporal's default retry policy and are each idempotent, so a retry
// is always safe (RL-2). The schedule-to-close bound turns an outage that
// outlasts it into an error the workflow routes to needs_attention, so no
// problem strands the workflow in planning or executing.
const (
	// smallStartToClose bounds one try. Approve clones the default branch
	// to read its head, the slowest of them.
	smallStartToClose = 5 * time.Minute
	// smallScheduleToClose bounds every try together.
	smallScheduleToClose = time.Hour
)

// smallOptions are the options of every activity but RunAttempt.
func smallOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    smallStartToClose,
		ScheduleToCloseTimeout: smallScheduleToClose,
	})
}

// Responses, the status comment and the command replies, are bounded
// tighter: they report on the run and are not part of it, so a GitHub
// outage gives up on them after responseScheduleToClose rather than hold
// the outbox for the hour a run step may take.
const (
	responseStartToClose    = time.Minute
	responseScheduleToClose = 10 * time.Minute
)

// responseOptions are the options of the activities the responder runs.
func responseOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    responseStartToClose,
		ScheduleToCloseTimeout: responseScheduleToClose,
	})
}

// attemptOptions are RunAttempt's: the attempt queue paired with the
// workflow's own, which the worker polls one at a time, a timeout computed by PrepareAttempt from
// configuration and recorded in history (decision 0012), a heartbeat
// timeout, and a single try. An attempt cannot restart: artifact_hash is
// write-once, and a second run would repeat the agent's spend. The
// workflow waits for a cancelled attempt to report, so the attempt records
// errored/cancelled and releases its sandbox before the workflow moves on.
func attemptOptions(ctx workflow.Context, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           AttemptQueue(workflow.GetInfo(ctx).TaskQueueName),
		StartToCloseTimeout: timeout,
		HeartbeatTimeout:    activity.HeartbeatTimeout,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
}

// cleanupContext is a context that survives the workflow's cancellation,
// for the writes and the release that must run on every exit path (RL-3).
// A cleanup activity started on the workflow's own context would never
// run once the workflow is cancelled.
func cleanupContext(ctx workflow.Context) workflow.Context {
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	return smallOptions(dctx)
}
