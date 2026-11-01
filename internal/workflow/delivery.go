package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
)

// versionDelivery gates ticket 9's delivery (decision 0014): a run that
// reads version 1 opens a pull request and ends delivered once CI
// passes; a run started before ticket 9 reads DefaultVersion and keeps
// the ticket 8 stub, which stops in needs_attention, so its history
// still replays.
const versionDelivery = "delivery-9"

// Delivery activity bounds. WritePullRequest makes one model call, which
// the router bounds at ten minutes (llm.MaxCallDuration), so its try is
// longer than that. Finalize builds sandboxes and runs the gate commands,
// as an attempt does, so it runs on the attempt queue with a heartbeat.
const (
	writeStartToClose    = 15 * time.Minute
	writeScheduleToClose = time.Hour
	finalizeStartToClose = 45 * time.Minute
	// finalizeTries is FR-SEC27's bound: a gating environment that cannot
	// be built is retried once and escalated on repeat.
	finalizeTries = 2
	// minPollEvery bounds how often CI is read, whatever the result of
	// OpenPullRequest says.
	minPollEvery = 10 * time.Second
)

// writeOptions are WritePullRequest's.
func writeOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    writeStartToClose,
		ScheduleToCloseTimeout: writeScheduleToClose,
	})
}

// finalizeOptions are Finalize's: the attempt queue, whose one slot keeps
// sandbox work one at a time, a heartbeat, and two tries. The workflow
// waits for a cancelled Finalize to release its sandboxes.
func finalizeOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           AttemptQueue(workflow.GetInfo(ctx).TaskQueueName),
		StartToCloseTimeout: finalizeStartToClose,
		HeartbeatTimeout:    activity.HeartbeatTimeout,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: finalizeTries},
	})
}

// deliver delivers a passed task. Under ticket 9 it opens the pull
// request and waits for CI (deliverPR); a run started before ticket 9
// runs the stub, which verifies the artifact and stops in
// needs_attention, never delivered.
func (f *featureRun) deliver(ctx workflow.Context, res TaskResult) error {
	if f.v9 {
		return f.deliverPR(ctx, res)
	}
	var out activity.DeliverResult
	err := workflow.ExecuteActivity(smallOptions(ctx), acts.Deliver, res.AttemptID, res.ArtifactHash, f.st.BaseSHA).Get(ctx, &out)
	if err != nil {
		return err
	}
	if out.Delivered {
		return f.transition(ctx, contracts.FeatureDelivered, "")
	}
	f.needsAttention(ctx, activity.FeatureReason(activity.ReasonError, out.Detail))
	return nil
}

// deliverPR runs the delivery sequence: write the pull request, land the
// artifact on the integration branch, rebase and re-gate it, open the
// draft, wait for CI, and mark it ready; then the feature request is
// delivered. A step that stops moves the feature request to
// needs_attention with its reason, and an error goes to the caller, which
// routes it there with reason error. /retry calls deliverPR again from
// the start; every activity resumes from the pull_requests row, so
// nothing already done is done twice. A /cancel between steps, or during
// the CI wait, ends delivery and the caller closes the run.
func (f *featureRun) deliverPR(ctx workflow.Context, res TaskResult) error {
	in := activity.DeliveryInput{
		AttemptID: res.AttemptID, TaskID: f.st.TaskID, ArtifactHash: res.ArtifactHash, BaseSHA: f.st.BaseSHA,
		Repo: f.in.Start.Repo, Issue: f.in.Start.Issue.Number,
	}
	steps := []struct {
		opts func(workflow.Context) workflow.Context
		fn   any
	}{
		{writeOptions, acts.WritePullRequest},
		{smallOptions, acts.Land},
		{finalizeOptions, acts.Finalize},
	}
	for _, s := range steps {
		var out activity.DeliveryStep
		if err := workflow.ExecuteActivity(s.opts(ctx), s.fn, in).Get(ctx, &out); err != nil {
			return err
		}
		if done := f.stopped(ctx, out); done {
			return nil
		}
	}
	var opened activity.OpenResult
	if err := workflow.ExecuteActivity(smallOptions(ctx), acts.OpenPullRequest, in).Get(ctx, &opened); err != nil {
		return err
	}
	if f.cancel != nil {
		return nil
	}
	passed, err := f.awaitCI(ctx, in, opened.CI)
	if err != nil || !passed {
		return err
	}
	var ready activity.DeliveryStep
	if err := workflow.ExecuteActivity(smallOptions(ctx), acts.MarkReady, in).Get(ctx, &ready); err != nil {
		return err
	}
	if done := f.stopped(ctx, ready); done {
		return nil
	}
	return f.transition(ctx, contracts.FeatureDelivered, "")
}

// stopped reports whether delivery ends after a step: the step stopped,
// and the feature request has been moved to needs_attention with its
// reason, or /cancel arrived while it ran.
func (f *featureRun) stopped(ctx workflow.Context, out activity.DeliveryStep) bool {
	if out.Stop != "" {
		f.needsAttention(ctx, activity.FeatureReason(out.Stop, out.Detail))
		return true
	}
	return f.cancel != nil
}

// awaitCI reads the required checks every ci.PollEvery until they have
// completed or ci.Timeout has passed since this entry, so a /retry after
// a CI stop waits the whole timeout again. It reports whether CI passed;
// a failure or a timeout has moved the feature request to
// needs_attention, and a /cancel has ended the wait. The wait is a
// durable timer: it holds no worker slot.
func (f *featureRun) awaitCI(ctx workflow.Context, in activity.DeliveryInput, ci activity.CIWait) (bool, error) {
	deadline := workflow.Now(ctx).Add(ci.Timeout)
	poll := max(ci.PollEvery, minPollEvery)
	for {
		var checks activity.ChecksResult
		if err := workflow.ExecuteActivity(smallOptions(ctx), acts.ReadChecks, in).Get(ctx, &checks); err != nil {
			return false, err
		}
		switch {
		case f.cancel != nil:
			return false, nil
		case checks.Completed && checks.Passed:
			return true, nil
		case checks.Completed:
			f.needsAttention(ctx, activity.FeatureReason(activity.ReasonCIFailed, checks.Detail))
			return false, nil
		case !workflow.Now(ctx).Before(deadline):
			f.needsAttention(ctx, activity.FeatureReason(activity.ReasonCITimeout, checks.Detail))
			return false, nil
		}
		if _, err := workflow.AwaitWithTimeout(ctx, poll, func() bool { return f.cancel != nil }); err != nil {
			return false, err
		}
		if f.cancel != nil {
			return false, nil
		}
	}
}
