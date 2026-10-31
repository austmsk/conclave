package workflow

import (
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// retry decides one /retry from needs_attention and returns the steps
// that resume the run, or nil when it is refused and the feature request
// stays where it is. The audit row records the final decision, so it is
// written here and not at receipt:
//   - the caps are checked first; at or over one, the reply names it, the
//     status comment shows it, and nothing moves;
//   - otherwise the feature request moves back to the phase it left, as
//     the owner, and the run resumes there: planning goes through analysis
//     and waits for /approve again (FR-S5); executing reopens the task at
//     its next ordinal, or delivers a passed task again.
func (f *featureRun) retry(ctx workflow.Context, sig github.CommandSignal) []step {
	var caps activity.CapCheck
	err := workflow.ExecuteActivity(smallOptions(ctx), acts.CheckCaps, f.st.FeatureRequestID).Get(ctx, &caps)
	switch {
	case ctx.Err() != nil || f.cancel != nil:
		f.reply(sig, activity.CommandRejected, activity.RejectCancelled, activity.ReplyCancelled)
		return nil
	case err != nil:
		f.refuseRetry(sig, "checking the caps: "+errorDetail(err))
		return nil
	case caps.Exceeded != "":
		f.overCap(sig, caps)
		return nil
	}
	return f.resume(ctx, sig)
}

// overCap refuses a /retry for a cap still exceeded: the reply names the
// cap and its figures, and the status comment shows them until the next
// move.
func (f *featureRun) overCap(sig github.CommandSignal, caps activity.CapCheck) {
	v := f.view()
	v.Cap = &caps
	f.enqueueCommand(commandEvent{
		ref: activity.CommandRefOf(sig), view: v, outcome: activity.CommandRejected,
		reason: activity.RejectOverCap + ":" + string(caps.Exceeded), reply: activity.ReplyOverCap,
	})
	f.refreshStatus(v)
}

// refuseRetry refuses a /retry that passed nothing it could act on: the
// caps could not be read, or the move out of needs_attention failed.
func (f *featureRun) refuseRetry(sig github.CommandSignal, detail string) {
	f.log.Error("/retry could not resume the run", "comment_id", sig.CommentID, "detail", detail)
	f.enqueueCommand(commandEvent{
		ref: activity.CommandRefOf(sig), view: f.view(), outcome: activity.CommandRejected,
		reason: activity.RejectResumeFailed, reply: activity.ReplyResumeFailed, detail: detail,
	})
}

// resume moves the feature request back to the phase it left and returns
// the steps that continue from there. needsAttention never records
// awaiting_approval as the phase left, so it is planning or executing.
func (f *featureRun) resume(ctx workflow.Context, sig github.CommandSignal) []step {
	left := f.st.PhaseLeft
	var steps []step
	switch left {
	case contracts.FeaturePlanning:
		steps = f.planSteps()
	case contracts.FeatureExecuting:
		steps = []step{f.resumeExecute}
	default:
		f.refuseRetry(sig, "the phase the run left is not recorded")
		return nil
	}
	decided := f.view()
	if err := f.transitionAs(ctx, left, "", store.ActorGitHub(sig.ActorID)); err != nil {
		f.refuseRetry(sig, "moving back to "+string(left)+": "+errorDetail(err))
		return nil
	}
	f.enqueueCommand(commandEvent{ref: activity.CommandRefOf(sig), view: decided, outcome: activity.CommandAccepted})
	f.refusePendingRetries()
	if left == contracts.FeaturePlanning {
		// The plan must be approved again: an /approve accepted before the
		// stop does not carry over (FR-S5).
		f.approval = nil
	}
	return steps
}
