package workflow

import (
	"slices"

	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
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

// validCommands is the command table in ARCHITECTURE.md, plus /help in
// every live status: the commands valid in each feature request status,
// in the table's order. It is the one table the signal gate, the /help
// reply and the status comment's "commands available" all read, so they
// cannot disagree. /approve is valid only in awaiting_approval, so it is
// never blind and never re-pins a base commit (FR-S5, FR-S19); /cancel in
// every status that is not terminal; /retry only in needs_attention.
// delivered lists /revise, which starts a linked run in a later milestone;
// the closed-run workflow answers it.
func validCommands(status contracts.FeatureRequestStatus) []github.CommandName {
	switch status {
	case contracts.FeaturePlanning:
		return []github.CommandName{github.CommandCancel, github.CommandAsk, github.CommandHelp}
	case contracts.FeatureAwaitingApproval:
		return []github.CommandName{github.CommandApprove, github.CommandRevise, github.CommandCancel, github.CommandAsk, github.CommandHelp}
	case contracts.FeatureExecuting:
		return []github.CommandName{github.CommandCancel, github.CommandStatus, github.CommandAsk, github.CommandHelp}
	case contracts.FeatureNeedsAttention:
		return []github.CommandName{github.CommandRetry, github.CommandRevise, github.CommandCancel, github.CommandAsk, github.CommandHelp}
	case contracts.FeatureDelivered:
		return []github.CommandName{github.CommandRevise}
	default:
		return nil
	}
}

// commandApplies reports whether the command table lists a command in a
// status. It is the single gate every command passes.
func commandApplies(name github.CommandName, status contracts.FeatureRequestStatus) bool {
	return slices.Contains(validCommands(status), name)
}

// notAvailableInM1 reports a command the table lists that no Milestone 1
// ticket implements: Milestone 1 has no planner to revise a plan or
// answer a question. It passes the gate and is answered "not available".
func notAvailableInM1(name github.CommandName) bool {
	return name == github.CommandRevise || name == github.CommandAsk
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

// onCommand decides one command against the status at receipt. A command
// from anyone but the owner, and a redelivered comment, are dropped with
// a log line and no audit row: the receiver already refuses both, so
// reaching here means a bug or a replay. A comment ID is remembered even
// when its command is refused, so a redelivery cannot apply later what was
// refused when it was written.
func (f *featureRun) onCommand(name github.CommandName, seen *recentIDs, sig github.CommandSignal) {
	logArgs := []any{"command", string(name), "comment_id", sig.CommentID, "status", string(f.st.Status)}
	switch {
	case sig.ActorID != f.in.OwnerUserID:
		f.log.Warn("dropped command from someone other than the owner", append(logArgs, "actor_id", sig.ActorID)...)
		return
	case seen.seen(sig.CommentID):
		f.log.Info("dropped redelivered command", logArgs...)
		return
	case !f.v8b:
		f.onCommandV0(name, sig, logArgs)
		return
	}
	switch {
	case !commandApplies(name, f.st.Status):
		f.reply(sig, activity.CommandRejected, activity.RejectNotValid, activity.ReplyNotValid)
	case notAvailableInM1(name):
		f.reply(sig, activity.CommandRejected, activity.RejectNotAvailable, activity.ReplyNotAvailable)
	default:
		f.accept(name, sig)
	}
}

// onCommandV0 is ticket 8's handling, kept for a run started before
// ticket 8b: /approve and /cancel act, and every other command is logged
// and dropped, with no reply and no audit, so its replay schedules what it
// scheduled then.
func (f *featureRun) onCommandV0(name github.CommandName, sig github.CommandSignal, logArgs []any) {
	if (name != github.CommandApprove && name != github.CommandCancel) || !commandApplies(name, f.st.Status) {
		f.log.Info("dropped command that does not apply in this status", logArgs...)
		return
	}
	f.acceptLatch(name, sig)
}

// accept acts on a command the table allows in the current status.
// /retry is audited by the owner wait, once its cap check has decided it.
func (f *featureRun) accept(name github.CommandName, sig github.CommandSignal) {
	switch name {
	case github.CommandApprove, github.CommandCancel:
		if !f.acceptLatch(name, sig) {
			f.reply(sig, activity.CommandRejected, activity.RejectAlreadyPending, activity.ReplyAlreadyPending)
			return
		}
		f.record(sig, activity.CommandAccepted, "")
	case github.CommandRetry:
		f.retries = append(f.retries, sig)
	case github.CommandStatus:
		f.reply(sig, activity.CommandAccepted, "", activity.ReplyStatus)
	case github.CommandHelp:
		f.reply(sig, activity.CommandAccepted, "", activity.ReplyHelp)
	}
}

// acceptLatch sets the pending /approve or /cancel, and reports false
// when one is already pending.
func (f *featureRun) acceptLatch(name github.CommandName, sig github.CommandSignal) bool {
	latch := &f.approval
	if name == github.CommandCancel {
		latch = &f.cancel
	}
	if *latch != nil {
		return false
	}
	*latch = &sig
	return true
}

// outboxItem is one response the responder sends, in order: a command's
// audit row and its reply, or a refresh of the status comment.
type outboxItem struct {
	command *commandEvent
	status  *activity.RunView
}

// commandEvent is one decided command: its audit row and, when reply is
// set, the reply it gets. view is the feature request as it stood when
// the command was decided.
type commandEvent struct {
	ref     activity.CommandRef
	view    activity.RunView
	outcome string
	reason  string
	reply   activity.ReplyKind
	detail  string
}

// view is the feature request as the owner is shown it now.
func (f *featureRun) view() activity.RunView {
	return activity.RunView{
		FeatureRequestID: f.st.FeatureRequestID, Status: f.st.Status, Reason: f.st.Reason,
		PhaseLeft: f.st.PhaseLeft, Commands: validCommands(f.st.Status),
	}
}

// record queues a command's audit row with no reply.
func (f *featureRun) record(sig github.CommandSignal, outcome, reason string) {
	f.enqueueCommand(commandEvent{ref: activity.CommandRefOf(sig), view: f.view(), outcome: outcome, reason: reason})
}

// reply queues a command's audit row and its reply.
func (f *featureRun) reply(sig github.CommandSignal, outcome, reason string, kind activity.ReplyKind) {
	f.enqueueCommand(commandEvent{ref: activity.CommandRefOf(sig), view: f.view(), outcome: outcome, reason: reason, reply: kind})
}

func (f *featureRun) enqueueCommand(ev commandEvent) {
	if !f.v8b {
		return
	}
	f.outbox = append(f.outbox, outboxItem{command: &ev})
}

// refreshStatus queues an update of the status comment to v.
func (f *featureRun) refreshStatus(v activity.RunView) {
	if !f.v8b || v.FeatureRequestID == "" {
		return
	}
	f.outbox = append(f.outbox, outboxItem{status: &v})
}

// startResponder starts the one coroutine that sends what the outbox
// holds, in order, so a reply never waits on the main flow and the main
// flow never waits on GitHub. It runs on a disconnected context: an
// accepted /cancel's audit row and the final status comment are still sent
// after Temporal cancels the workflow. It stops once drain asks it to and
// the outbox is empty.
func (f *featureRun) startResponder(ctx workflow.Context) {
	if !f.v8b {
		f.responderDone = true
		return
	}
	rctx, _ := workflow.NewDisconnectedContext(ctx)
	workflow.Go(rctx, func(ctx workflow.Context) {
		defer func() { f.responderDone = true }()
		for {
			_ = workflow.Await(ctx, func() bool { return len(f.outbox) > 0 || f.closing })
			if len(f.outbox) == 0 {
				return
			}
			item := f.outbox[0]
			f.outbox = f.outbox[1:]
			f.send(ctx, item)
		}
	})
}

// send runs one outbox item's activities. A status refresh that a later
// one in the outbox supersedes is skipped. A failure is logged and never
// routes the feature request: the response is about the run, not part
// of it.
func (f *featureRun) send(ctx workflow.Context, item outboxItem) {
	actx := responseOptions(ctx)
	if item.status != nil {
		if f.statusQueued() {
			return
		}
		err := workflow.ExecuteActivity(actx, acts.UpsertStatusComment, activity.StatusCommentInput{
			Repo: f.in.Start.Repo, Issue: f.in.Start.Issue.Number, View: *item.status,
		}).Get(ctx, nil)
		if err != nil {
			f.log.Error("updating the status comment", "error", err)
		}
		return
	}
	ev := item.command
	err := workflow.ExecuteActivity(actx, acts.RecordCommand, activity.RecordCommandInput{
		Ref: ev.ref, FeatureRequestID: ev.view.FeatureRequestID, Status: ev.view.Status, Outcome: ev.outcome, Reason: ev.reason,
	}).Get(ctx, nil)
	if err != nil {
		f.log.Error("recording command", "comment_id", ev.ref.CommentID, "error", err)
	}
	if ev.reply == "" {
		return
	}
	err = workflow.ExecuteActivity(actx, acts.ReplyToCommand, activity.ReplyInput{
		Ref: ev.ref, Kind: ev.reply, View: ev.view, Detail: ev.detail,
	}).Get(ctx, nil)
	if err != nil {
		f.log.Error("replying to command", "comment_id", ev.ref.CommentID, "error", err)
	}
}

// statusQueued reports whether the outbox still holds a status refresh.
func (f *featureRun) statusQueued() bool {
	return slices.ContainsFunc(f.outbox, func(it outboxItem) bool { return it.status != nil })
}

// drain refuses every /retry still waiting, overtaken by the run's end,
// then waits for the responder to send everything queued, on a context
// that survives cancellation. Every return of FeatureWorkflow passes
// through it, so a decided command always has its audit row and its reply
// before the workflow closes.
func (f *featureRun) drain(ctx workflow.Context) {
	if !f.v8b {
		return
	}
	f.refuseRetriesForCancel()
	f.closing = true
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	_ = workflow.Await(dctx, func() bool { return f.responderDone })
}

// refuseRetriesForCancel answers every /retry still queued that a cancel
// overtook: the reply says the cancel takes precedence, and the row is
// rejected cancelled.
func (f *featureRun) refuseRetriesForCancel() {
	for _, sig := range f.retries {
		f.reply(sig, activity.CommandRejected, activity.RejectCancelled, activity.ReplyCancelled)
	}
	f.retries = nil
}

// refusePendingRetries answers every /retry still queued once the feature
// request has left needs_attention: in the status it is in now, /retry is
// not valid.
func (f *featureRun) refusePendingRetries() {
	for _, sig := range f.retries {
		f.reply(sig, activity.CommandRejected, activity.RejectNotValid, activity.ReplyNotValid)
	}
	f.retries = nil
}
