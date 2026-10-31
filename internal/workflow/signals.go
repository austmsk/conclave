package workflow

import (
	"slices"

	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
)

// recentIDs is a bounded set of comment IDs, oldest dropped first. A
// redelivered or replayed webhook carries the comment ID of the command it
// repeats, so a handler that has seen the ID ignores it.
type recentIDs struct {
	ids []int64
}

// seen reports whether id is in the set, and adds it if not.
func (r *recentIDs) seen(id int64) bool {
	if slices.Contains(r.ids, id) {
		return true
	}
	if len(r.ids) == maxRecentComments {
		r.ids = r.ids[1:]
	}
	r.ids = append(r.ids, id)
	return false
}

// commandApplies reports whether a command acts on a feature request in
// the given status. It is the single gate for every command, so the rule
// is testable on its own: /approve only in awaiting_approval, so it is
// never blind and never re-pins a base commit (FR-S5, FR-S19); /cancel in
// any status that is not terminal. Every other command waits for ticket
// 8b and is dropped.
func commandApplies(name github.CommandName, status contracts.FeatureRequestStatus) bool {
	switch name {
	case github.CommandApprove:
		return status == contracts.FeatureAwaitingApproval
	case github.CommandCancel:
		return status.CanMoveTo(contracts.FeatureCancelled)
	default:
		return false
	}
}

// listen starts one receiver per command signal, in commandNames' fixed
// order. Each handler runs in the workflow's own scheduler, so the status
// it checks is the status at that moment and the decision replays the
// same way.
func (f *featureRun) listen(ctx workflow.Context) {
	for _, name := range commandNames {
		ch := workflow.GetSignalChannel(ctx, SignalName(name))
		seen := &recentIDs{}
		workflow.Go(ctx, func(ctx workflow.Context) {
			for {
				var sig github.CommandSignal
				if more := ch.Receive(ctx, &sig); !more {
					return
				}
				f.onCommand(name, seen, sig)
			}
		})
	}
}

// onCommand applies or drops one command. A command from anyone but the
// owner, a redelivered comment, and a command that does not apply in the
// current status are dropped with a log line; ticket 8b replies to them.
// A comment ID is remembered even when its command is dropped, so a
// redelivery cannot apply later what was refused when it was written.
func (f *featureRun) onCommand(name github.CommandName, seen *recentIDs, sig github.CommandSignal) {
	logArgs := []any{"command", string(name), "comment_id", sig.CommentID, "status", string(f.st.Status)}
	switch {
	case sig.ActorID != f.in.OwnerUserID:
		f.log.Warn("dropped command from someone other than the owner", append(logArgs, "actor_id", sig.ActorID)...)
		return
	case seen.seen(sig.CommentID):
		f.log.Info("dropped redelivered command", logArgs...)
		return
	case !commandApplies(name, f.st.Status):
		f.log.Info("dropped command that does not apply in this status", logArgs...)
		return
	}
	switch name {
	case github.CommandApprove:
		if f.approval == nil {
			f.approval = &sig
		}
	case github.CommandCancel:
		if f.cancel == nil {
			f.cancel = &sig
		}
	}
}
