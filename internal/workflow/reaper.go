package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
)

// The scheduled reaper (NFR-R3, decision 0015). The worker keeps a
// Temporal schedule with this ID that starts ReaperWorkflow every
// interval; `conclave halt` pauses it and `conclave resume` unpauses it.
const (
	ReaperScheduleID = "conclave-reaper"
	// ReaperWorkflowID is the ID each scheduled run is started under;
	// Temporal appends the scheduled time, so runs never collide.
	ReaperWorkflowID = "reaper"
)

// ReaperInput is the cutoff each scheduled sweep uses.
type ReaperInput struct {
	OlderThan time.Duration
}

// The sweep's bounds: a Docker listing and a removal per orphan, a few
// tries, and then the next scheduled run is the retry.
const (
	reaperStartToClose    = 10 * time.Minute
	reaperScheduleToClose = 30 * time.Minute
)

// ReaperWorkflow runs one sweep: release every recorded sandbox older
// than the cutoff that was never marked released, then remove unrecorded
// sandbox containers older than it. The schedule skips a run while the
// previous one is still going, so two sweeps never overlap.
func ReaperWorkflow(ctx workflow.Context, in ReaperInput) (activity.SweepResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    reaperStartToClose,
		ScheduleToCloseTimeout: reaperScheduleToClose,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	var res activity.SweepResult
	err := workflow.ExecuteActivity(ctx, acts.SweepOrphans, activity.SweepInput{OlderThan: in.OlderThan}).Get(ctx, &res)
	return res, err
}
