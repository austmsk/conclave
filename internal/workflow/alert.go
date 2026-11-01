package workflow

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
)

// versionAlerts gates ticket 10a's pause alert (decision 0015): a run that
// reads version 1 raises an alert when a budget cap stops it; a run
// started before 10a reads DefaultVersion and raises none, so its history
// still replays.
const versionAlerts = "alerts-10"

// The alert's bounds. An alert reports on the run and is not part of it,
// so, like a response, it gives up after alertScheduleToClose rather than
// hold the needs_attention wait while GitHub is down; the local alert log
// already holds it from the first try.
const (
	alertStartToClose    = time.Minute
	alertScheduleToClose = 10 * time.Minute
)

func alertOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    alertStartToClose,
		ScheduleToCloseTimeout: alertScheduleToClose,
	})
}

// raiseAlert raises the pause alert after a move to needs_attention whose
// reason is a budget cap (FR-SEC31). A failure is logged and never stops
// the run: the owner still sees the stop on the issue's status comment.
func (f *featureRun) raiseAlert(ctx workflow.Context, reason string) {
	if !f.v10 || !activity.IsBudgetCapReason(reason) {
		return
	}
	in := activity.AlertInput{
		FeatureRequestID: f.st.FeatureRequestID, WorkflowID: f.in.Start.WorkflowID,
		Reason: reason, Revision: f.frRev,
	}
	if err := workflow.ExecuteActivity(alertOptions(ctx), acts.RaiseAlert, in).Get(ctx, nil); err != nil {
		f.log.Error("raising the budget cap alert", "reason", reason, "error", err)
	}
}
