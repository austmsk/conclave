package workflow

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
)

// ClosedCommandInput is a command that arrived for an issue with no
// running feature workflow, and the owner whose commands count.
type ClosedCommandInput struct {
	Signal      github.CommandSignal
	OwnerUserID int64
}

// ClosedCommandWorkflow answers a command the receiver could not signal
// because no feature workflow was running for the issue: its run is
// delivered or cancelled, it never started, or its workflow ended while
// the feature request was still live. The receiver holds no database
// access and no App key, so it starts this short workflow on the worker
// instead of answering itself. The command is rejected, replied to, and
// audited; nothing else changes.
//
// A command already in the audit, decided by the live workflow and
// delivered again after the run closed, is left alone: it has its reply
// and its row.
func ClosedCommandWorkflow(ctx workflow.Context, in ClosedCommandInput) error {
	log := workflow.GetLogger(ctx)
	sig := in.Signal
	if sig.ActorID != in.OwnerUserID {
		log.Warn("dropped command from someone other than the owner", "comment_id", sig.CommentID, "actor_id", sig.ActorID)
		return nil
	}
	actx := smallOptions(ctx)
	var fr activity.LookupFeatureResult
	err := workflow.ExecuteActivity(actx, acts.LookupFeature, activity.LookupFeatureInput{
		WorkflowID: sig.WorkflowID, CommentID: sig.CommentID,
	}).Get(ctx, &fr)
	if err != nil {
		return err
	}
	if fr.Recorded {
		log.Info("command already decided by its run", "comment_id", sig.CommentID)
		return nil
	}
	kind, reason := closedDecision(sig.Command.Name, fr)
	ref := activity.CommandRefOf(sig)
	view := activity.RunView{FeatureRequestID: fr.FeatureRequestID, Status: fr.Status, Reason: fr.Reason}
	recordErr := workflow.ExecuteActivity(actx, acts.RecordCommand, activity.RecordCommandInput{
		Ref: ref, FeatureRequestID: fr.FeatureRequestID, Status: fr.Status, Outcome: activity.CommandRejected, Reason: reason,
	}).Get(ctx, nil)
	replyErr := workflow.ExecuteActivity(responseOptions(ctx), acts.ReplyToCommand, activity.ReplyInput{
		Ref: ref, Kind: kind, View: view,
	}).Get(ctx, nil)
	return errors.Join(recordErr, replyErr)
}

// closedDecision is why a command to a closed run is rejected and how the
// reply says so. /revise on a delivered run is in the command table, so it
// gets the not-available answer, as it does in a live run.
func closedDecision(name github.CommandName, fr activity.LookupFeatureResult) (activity.ReplyKind, string) {
	switch {
	case !fr.Found:
		return activity.ReplyNoRun, activity.RejectNoRun
	case fr.Status == contracts.FeatureDelivered && commandApplies(name, fr.Status) && notAvailableInM1(name):
		return activity.ReplyClosedRun, activity.RejectNotAvailable
	case fr.Status == contracts.FeatureDelivered, fr.Status == contracts.FeatureCancelled:
		return activity.ReplyClosedRun, activity.RejectClosedRun
	default:
		return activity.ReplyNoWorkflow, activity.RejectNoWorkflow
	}
}
