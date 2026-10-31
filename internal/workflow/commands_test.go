package workflow

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// Comment IDs of the commands that drive a scenario; the commands under
// test use IDs from testCommentBase.
const (
	approveID       = int64(100)
	cancelID        = int64(101)
	testCommentBase = int64(200)
)

// stopDelivery is the move the ticket 8 deliver stub ends with.
const stopDelivery = "executing>needs_attention error:" + activity.DetailDeliveryStub

// scenario is a run driven into one live status, with commands sent while
// it is there. The run always ends with a /cancel, so it completes.
type scenario struct {
	w  *world
	te *testsuite.TestWorkflowEnvironment
}

// inStatus starts a run that reaches status and receives cmds there:
//   - planning: while Analyze is still running;
//   - awaiting_approval: an hour in, with no /approve;
//   - executing: from inside the attempt;
//   - needs_attention: after the attempt failed.
func inStatus(t *testing.T, status contracts.FeatureRequestStatus, cmds ...github.CommandSignal) scenario {
	t.Helper()
	w := newWorld()
	w.run = returns(contracts.AttemptFailed, contracts.ReasonGateFailed)
	te := newEnv(t, w)
	send := func() {
		for _, sig := range cmds {
			te.SignalWorkflow(SignalName(sig.Command.Name), sig)
		}
	}
	switch status {
	case contracts.FeaturePlanning:
		w.duringAnalyze = send
	case contracts.FeatureAwaitingApproval:
		te.RegisterDelayedCallback(send, time.Hour)
	case contracts.FeatureExecuting:
		signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
		w.run = func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
			send()
			return w.finish(n, contracts.AttemptFailed, contracts.ReasonGateFailed), nil
		}
	case contracts.FeatureNeedsAttention:
		signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
		te.RegisterDelayedCallback(send, 2*time.Hour)
	default:
		t.Fatalf("no scenario for %s", status)
	}
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	return scenario{w: w, te: te}
}

// run executes the scenario's workflow to completion.
func (s scenario) run(t *testing.T) *world {
	t.Helper()
	s.te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, s.te)
	return s.w
}

// baselineMoves is what a scenario does with no commands under test.
func baselineMoves(t *testing.T, status contracts.FeatureRequestStatus) []string {
	t.Helper()
	return inStatus(t, status).run(t).moves()
}

// notIn is every command the table does not list in status.
func notIn(status contracts.FeatureRequestStatus) []github.CommandName {
	var out []github.CommandName
	for _, name := range commandNames {
		if !slices.Contains(commandTable[status], name) {
			out = append(out, name)
		}
	}
	return out
}

// commandsFrom numbers one command per name from testCommentBase.
func commandsFrom(names []github.CommandName) []github.CommandSignal {
	out := make([]github.CommandSignal, len(names))
	for i, name := range names {
		out[i] = command(name, testCommentBase+int64(i))
	}
	return out
}

// wantAudit checks one command's audit row.
func wantAudit(t *testing.T, w *world, id int64, status contracts.FeatureRequestStatus, outcome, reason string) {
	t.Helper()
	row, ok := w.auditRow(id)
	if !ok {
		t.Fatalf("comment %d has no audit row", id)
	}
	equal(t, "audited status", row.Status, status)
	equal(t, "audited outcome", row.Outcome, outcome)
	equal(t, "audited reason", row.Reason, reason)
}

// wantReply checks one command's reply.
func wantReply(t *testing.T, w *world, id int64, kind activity.ReplyKind) activity.ReplyInput {
	t.Helper()
	r, ok := w.reply(id)
	if !ok {
		t.Fatalf("comment %d got no reply", id)
	}
	equal(t, "reply kind", r.Kind, kind)
	return r
}

func names(cmds []github.CommandName) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = string(c)
	}
	return out
}

// TestInvalidCommandPerLiveStatus sends, in each live status, every
// command the table does not list there: each gets a reply and a rejected
// audit row, and the run moves exactly as it does without them.
func TestInvalidCommandPerLiveStatus(t *testing.T) {
	for _, status := range liveStatuses {
		t.Run(string(status), func(t *testing.T) {
			cmds := commandsFrom(notIn(status))
			w := inStatus(t, status, cmds...).run(t)
			for _, sig := range cmds {
				r := wantReply(t, w, sig.CommentID, activity.ReplyNotValid)
				equal(t, "reply status", r.View.Status, status)
				wantAudit(t, w, sig.CommentID, status, activity.CommandRejected, activity.RejectNotValid)
			}
			equalSlices(t, "moves", w.moves(), baselineMoves(t, status))
			wantApprovals := 0
			if status == contracts.FeatureExecuting || status == contracts.FeatureNeedsAttention {
				wantApprovals = 1
			}
			equal(t, "approvals", w.approved, wantApprovals)
		})
	}
}

// TestRevAskNotAvailable sends /revise and /ask where the table lists
// them: Milestone 1 implements neither, so each is answered "not
// available", audited as rejected, and changes nothing.
func TestRevAskNotAvailable(t *testing.T) {
	for _, status := range liveStatuses {
		t.Run(string(status), func(t *testing.T) {
			var listed []github.CommandName
			for _, name := range []github.CommandName{github.CommandRevise, github.CommandAsk} {
				if slices.Contains(commandTable[status], name) {
					listed = append(listed, name)
				}
			}
			cmds := commandsFrom(listed)
			w := inStatus(t, status, cmds...).run(t)
			for _, sig := range cmds {
				wantReply(t, w, sig.CommentID, activity.ReplyNotAvailable)
				wantAudit(t, w, sig.CommentID, status, activity.CommandRejected, activity.RejectNotAvailable)
			}
			equalSlices(t, "moves", w.moves(), baselineMoves(t, status))
		})
	}
}

// TestStatusReply sends /status while executing: the reply reports the
// status and the commands available, and the command is accepted.
func TestStatusReply(t *testing.T) {
	sig := command(github.CommandStatus, testCommentBase)
	w := inStatus(t, contracts.FeatureExecuting, sig).run(t)
	r := wantReply(t, w, sig.CommentID, activity.ReplyStatus)
	equal(t, "reported status", r.View.Status, contracts.FeatureExecuting)
	equal(t, "reported feature request", r.View.FeatureRequestID, frID)
	equalSlices(t, "reported commands", names(r.View.Commands), names(commandTable[contracts.FeatureExecuting]))
	wantAudit(t, w, sig.CommentID, contracts.FeatureExecuting, activity.CommandAccepted, "")
}

// TestHelpListsValidCommands sends /help in each live status: the reply
// lists exactly the commands the table allows there, in its order.
func TestHelpListsValidCommands(t *testing.T) {
	for _, status := range liveStatuses {
		t.Run(string(status), func(t *testing.T) {
			sig := command(github.CommandHelp, testCommentBase)
			w := inStatus(t, status, sig).run(t)
			r := wantReply(t, w, sig.CommentID, activity.ReplyHelp)
			equalSlices(t, "listed commands", names(r.View.Commands), names(commandTable[status]))
			wantAudit(t, w, sig.CommentID, status, activity.CommandAccepted, "")
		})
	}
}

// TestRetryAfterCapRaised stops a run on the feature request cap, then
// sends /retry once the cap is no longer exceeded, as after the owner
// raised it and restarted the worker. The run continues in executing at
// ordinal 2, and attempt 1 keeps its recorded outcome.
func TestRetryAfterCapRaised(t *testing.T) {
	w := newWorld()
	w.run = func(ctx context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		if n == 1 {
			return budgetRefusal(llm.BudgetFeatureRequest)(ctx, w, n)
		}
		return w.finish(n, contracts.AttemptPassed, ""), nil
	}
	te := newEnv(t, w)
	retry := command(github.CommandRetry, testCommentBase)
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(te, 2*time.Hour, retry)
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equalSlices(t, "moves", w.moves(), []string{
		toApproval, toExecuting, "executing>needs_attention budget_cap:feature_request",
		"needs_attention>executing", stopDelivery, toCancelled,
	})
	equalSlices(t, "attempts", w.attemptStates(), []string{"errored/budget_cap", "passed/"})
	equal(t, "RunAttempt calls", w.called("RunAttempt"), 2)
	equal(t, "CreateTask calls", w.called("CreateTask"), 1)
	wantAudit(t, w, retry.CommentID, contracts.FeatureNeedsAttention, activity.CommandAccepted, "")
	if _, replied := w.reply(retry.CommentID); replied {
		t.Error("an accepted /retry got a reply; the status comment reports it")
	}
}

// TestRetryStillOverCap sends /retry while the feature request is still
// at its cap: the reply names the cap, the status comment shows it, the
// audit row is rejected over_cap:feature_request, and the run stays in
// needs_attention with no new attempt.
func TestRetryStillOverCap(t *testing.T) {
	w := newWorld()
	w.run = budgetRefusal(llm.BudgetFeatureRequest)
	w.caps = activity.CapCheck{Exceeded: llm.BudgetFeatureRequest, FeatureRequestTokens: 1000, FeatureRequestCap: 1000}
	te := newEnv(t, w)
	retry := command(github.CommandRetry, testCommentBase)
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(te, 2*time.Hour, retry)
	var between Status
	var shown *activity.RunView
	te.RegisterDelayedCallback(func() {
		between = queryStatus(t, te)
		w.mu.Lock()
		shown = w.status
		w.mu.Unlock()
	}, 3*time.Hour)
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equal(t, "status after the refusal", between.Status, contracts.FeatureNeedsAttention)
	equal(t, "reason after the refusal", between.Reason, "budget_cap:feature_request")
	r := wantReply(t, w, retry.CommentID, activity.ReplyOverCap)
	if r.View.Cap == nil || r.View.Cap.Exceeded != llm.BudgetFeatureRequest {
		t.Errorf("reply cap = %+v, want the feature request cap", r.View.Cap)
	}
	if shown == nil || shown.Cap == nil || shown.Cap.Exceeded != llm.BudgetFeatureRequest {
		t.Errorf("status comment after the refusal = %+v, want it to name the feature request cap", shown)
	}
	wantAudit(t, w, retry.CommentID, contracts.FeatureNeedsAttention, activity.CommandRejected, "over_cap:feature_request")
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, "executing>needs_attention budget_cap:feature_request", toCancelled})
	equal(t, "RunAttempt calls", w.called("RunAttempt"), 1)
	equal(t, "ResumeTask calls", w.called("ResumeTask"), 0)
}

// TestRetryFromPlanningWaitsForApprove fails the plan comment once, so
// the run stops in needs_attention having left planning. /retry returns
// to planning and posts the plan again, and the run then waits on
// /approve: no task exists until one arrives (FR-S5).
func TestRetryFromPlanningWaitsForApprove(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptFailed, contracts.ReasonGateFailed)
	te := newEnv(t, w)
	var mu sync.Mutex
	tries := 0
	te.RegisterActivityWithOptions(func(ctx context.Context, in activity.PlanCommentInput) (activity.PlanCommentResult, error) {
		mu.Lock()
		tries++
		first := tries == 1
		mu.Unlock()
		if first {
			return activity.PlanCommentResult{}, temporal.NewNonRetryableApplicationError("comments locked", activity.ErrTypePermanent, nil)
		}
		return w.PostPlanComment(ctx, in)
	}, sdkRegister("PostPlanComment"))
	signalAfter(te, time.Hour, command(github.CommandRetry, testCommentBase))
	var waiting Status
	tasksWhileWaiting := -1
	te.RegisterDelayedCallback(func() {
		waiting = queryStatus(t, te)
		tasksWhileWaiting = w.called("CreateTask")
	}, 5*time.Hour)
	signalAfter(te, 6*time.Hour, command(github.CommandApprove, approveID))
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equal(t, "status after /retry", waiting.Status, contracts.FeatureAwaitingApproval)
	equal(t, "tasks before /approve", tasksWhileWaiting, 0)
	equal(t, "plan comments posted", w.comments, 1)
	equal(t, "approvals", w.approved, 1)
	moves := w.moves()
	if len(moves) < 5 || moves[0] != toApproval || moves[1] != "awaiting_approval>planning" ||
		moves[3] != "needs_attention>planning" || moves[4] != toApproval {
		t.Errorf("moves = %q, want a stop from planning, a return to planning, and awaiting_approval again", moves)
	}
}

// TestRetryAfterDeliverStop stops a run whose task passed at the
// delivery stub; /retry delivers the passed task again and runs no new
// attempt.
func TestRetryAfterDeliverStop(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	te := newEnv(t, w)
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(te, 2*time.Hour, command(github.CommandRetry, testCommentBase))
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equal(t, "Deliver calls", w.called("Deliver"), 2)
	equal(t, "RunAttempt calls", w.called("RunAttempt"), 1)
	equal(t, "ResumeTask calls", w.called("ResumeTask"), 0)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, stopDelivery, "needs_attention>executing", stopDelivery, toCancelled})
}

// TestSecondRetryRefusedAfterResume sends two /retry comments at once:
// the first resumes the run, and the second, decided after the run has
// left needs_attention, is refused rather than saved for the next stop.
func TestSecondRetryRefusedAfterResume(t *testing.T) {
	w := newWorld()
	w.run = func(ctx context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		if n == 1 {
			return budgetRefusal(llm.BudgetMonth)(ctx, w, n)
		}
		return w.finish(n, contracts.AttemptFailed, contracts.ReasonGateFailed), nil
	}
	te := newEnv(t, w)
	first, second := command(github.CommandRetry, testCommentBase), command(github.CommandRetry, testCommentBase+1)
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(te, 2*time.Hour, first)
	signalAfter(te, 2*time.Hour, second)
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	wantAudit(t, w, first.CommentID, contracts.FeatureNeedsAttention, activity.CommandAccepted, "")
	wantAudit(t, w, second.CommentID, contracts.FeatureExecuting, activity.CommandRejected, activity.RejectNotValid)
	wantReply(t, w, second.CommentID, activity.ReplyNotValid)
	equal(t, "RunAttempt calls", w.called("RunAttempt"), 2)
	equalSlices(t, "moves", w.moves(), []string{
		toApproval, toExecuting, "executing>needs_attention budget_cap:month",
		"needs_attention>executing", "executing>needs_attention attempt_failed", toCancelled,
	})
}

// TestStatusCommentEditedInPlace runs through several moves: the issue
// holds exactly one status comment, posted once and edited after, showing
// the last status.
func TestStatusCommentEditedInPlace(t *testing.T) {
	w := inStatus(t, contracts.FeatureNeedsAttention).run(t)
	equal(t, "status comments posted", w.statusPosts, 1)
	if w.statusEdits < 2 {
		t.Errorf("status comment edits = %d, want one per later move", w.statusEdits)
	}
	if w.status == nil {
		t.Fatal("no status comment")
	}
	equal(t, "last status shown", w.status.Status, contracts.FeatureCancelled)
	equal(t, "commands shown when cancelled", len(w.status.Commands), 0)
}

// TestStatusCommentNamesReason checks what the comment shows in
// needs_attention: the reason with the cap, the phase left, and the
// commands available there.
func TestStatusCommentNamesReason(t *testing.T) {
	w := newWorld()
	w.run = budgetRefusal(llm.BudgetMonth)
	te := newEnv(t, w)
	var shown activity.RunView
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	te.RegisterDelayedCallback(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.status != nil {
			shown = *w.status
		}
	}, 2*time.Hour)
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equal(t, "status shown", shown.Status, contracts.FeatureNeedsAttention)
	equal(t, "reason shown", shown.Reason, "budget_cap:month")
	equal(t, "phase left shown", shown.PhaseLeft, contracts.FeatureExecuting)
	equalSlices(t, "commands shown", names(shown.Commands), names(commandTable[contracts.FeatureNeedsAttention]))
}

// TestEveryCommandAudited sends accepted and rejected commands, a
// redelivered one, and one from a stranger, while awaiting approval: each
// owner comment has exactly one audit row, and the stranger's has none.
func TestEveryCommandAudited(t *testing.T) {
	stranger := command(github.CommandCancel, testCommentBase+9)
	stranger.ActorID = ownerID + 1
	cmds := []github.CommandSignal{
		command(github.CommandHelp, testCommentBase),
		command(github.CommandHelp, testCommentBase),
		command(github.CommandStatus, testCommentBase+1),
		command(github.CommandApprove, testCommentBase+2),
		command(github.CommandApprove, testCommentBase+3),
		stranger,
	}
	w := inStatus(t, contracts.FeatureAwaitingApproval, cmds...).run(t)

	w.mu.Lock()
	audited := slices.Clone(w.auditOrder)
	w.mu.Unlock()
	slices.Sort(audited)
	want := []int64{cancelID, testCommentBase, testCommentBase + 1, testCommentBase + 2, testCommentBase + 3}
	if !slices.Equal(audited, want) {
		t.Fatalf("audited comment IDs = %v, want %v", audited, want)
	}
	equal(t, "RecordCommand calls", w.called("RecordCommand"), len(want))
	wantAudit(t, w, testCommentBase, contracts.FeatureAwaitingApproval, activity.CommandAccepted, "")
	wantAudit(t, w, testCommentBase+1, contracts.FeatureAwaitingApproval, activity.CommandRejected, activity.RejectNotValid)
	wantAudit(t, w, testCommentBase+2, contracts.FeatureAwaitingApproval, activity.CommandAccepted, "")
	wantAudit(t, w, testCommentBase+3, contracts.FeatureAwaitingApproval, activity.CommandRejected, activity.RejectAlreadyPending)
	wantAudit(t, w, cancelID, contracts.FeatureNeedsAttention, activity.CommandAccepted, "")
	equal(t, "approvals", w.approved, 1)
}

// TestCancelAuditedBeforeClose ends the workflow right after a command:
// by /cancel, and by Temporal cancelling the workflow. Either way the
// command's audit row is written before the workflow closes.
func TestCancelAuditedBeforeClose(t *testing.T) {
	t.Run("/cancel", func(t *testing.T) {
		w := newWorld()
		te := newEnv(t, w)
		signalAfter(te, time.Hour, command(github.CommandCancel, cancelID))
		te.ExecuteWorkflow(FeatureWorkflow, featureInput())
		checkCompleted(t, te)
		wantAudit(t, w, cancelID, contracts.FeatureAwaitingApproval, activity.CommandAccepted, "")
		equal(t, "last status shown", w.status.Status, contracts.FeatureCancelled)
	})
	t.Run("workflow cancelled", func(t *testing.T) {
		w := newWorld()
		te := newEnv(t, w)
		signalAfter(te, time.Hour, command(github.CommandHelp, testCommentBase))
		te.RegisterDelayedCallback(te.CancelWorkflow, time.Hour+time.Second)
		te.ExecuteWorkflow(FeatureWorkflow, featureInput())
		checkCompleted(t, te)
		wantAudit(t, w, testCommentBase, contracts.FeatureAwaitingApproval, activity.CommandAccepted, "")
		wantReply(t, w, testCommentBase, activity.ReplyHelp)
		equal(t, "last status shown", w.status.Status, contracts.FeatureCancelled)
	})
}

// TestClosedCommandWorkflow answers a command no feature workflow was
// running to take, for each case the lookup can find.
func TestClosedCommandWorkflow(t *testing.T) {
	found := func(st contracts.FeatureRequestStatus) activity.LookupFeatureResult {
		return activity.LookupFeatureResult{Found: true, FeatureRequestID: frID, Status: st}
	}
	tests := []struct {
		name       string
		cmd        github.CommandName
		lookup     activity.LookupFeatureResult
		wantReply  activity.ReplyKind
		wantReason string
	}{
		{"delivered", github.CommandRetry, found(contracts.FeatureDelivered), activity.ReplyClosedRun, activity.RejectClosedRun},
		{"/revise on delivered", github.CommandRevise, found(contracts.FeatureDelivered), activity.ReplyClosedRun, activity.RejectNotAvailable},
		{"cancelled", github.CommandCancel, found(contracts.FeatureCancelled), activity.ReplyClosedRun, activity.RejectClosedRun},
		{"no run", github.CommandStatus, activity.LookupFeatureResult{}, activity.ReplyNoRun, activity.RejectNoRun},
		{"live without a workflow", github.CommandRetry, found(contracts.FeatureNeedsAttention), activity.ReplyNoWorkflow, activity.RejectNoWorkflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			w.lookup = tt.lookup
			te := newEnv(t, w)
			sig := command(tt.cmd, testCommentBase)
			te.ExecuteWorkflow(ClosedCommandWorkflow, ClosedCommandInput{Signal: sig, OwnerUserID: ownerID})
			checkCompleted(t, te)
			r := wantReply(t, w, sig.CommentID, tt.wantReply)
			equal(t, "reply status", r.View.Status, tt.lookup.Status)
			wantAudit(t, w, sig.CommentID, tt.lookup.Status, activity.CommandRejected, tt.wantReason)
			equalSlices(t, "moves", w.moves(), nil)
		})
	}
}

// TestClosedCommandWorkflowIgnores leaves alone a command from someone
// other than the owner, and one already audited by its live run.
func TestClosedCommandWorkflowIgnores(t *testing.T) {
	t.Run("stranger", func(t *testing.T) {
		w := newWorld()
		te := newEnv(t, w)
		sig := command(github.CommandCancel, testCommentBase)
		sig.ActorID = ownerID + 1
		te.ExecuteWorkflow(ClosedCommandWorkflow, ClosedCommandInput{Signal: sig, OwnerUserID: ownerID})
		checkCompleted(t, te)
		equal(t, "LookupFeature calls", w.called("LookupFeature"), 0)
		equal(t, "RecordCommand calls", w.called("RecordCommand"), 0)
	})
	t.Run("already audited", func(t *testing.T) {
		w := newWorld()
		w.lookup = activity.LookupFeatureResult{Found: true, FeatureRequestID: frID, Status: contracts.FeatureCancelled}
		sig := command(github.CommandCancel, testCommentBase)
		w.audit[sig.CommentID] = activity.RecordCommandInput{Outcome: activity.CommandAccepted}
		te := newEnv(t, w)
		te.ExecuteWorkflow(ClosedCommandWorkflow, ClosedCommandInput{Signal: sig, OwnerUserID: ownerID})
		checkCompleted(t, te)
		equal(t, "ReplyToCommand calls", w.called("ReplyToCommand"), 0)
		equal(t, "RecordCommand calls", w.called("RecordCommand"), 0)
	})
}

// TestRetryAfterFailedApproveNeedsNewApprove has the /approve accepted
// and the approval itself fail, so the run stops having left planning.
// After /retry the plan waits for a new /approve: the one accepted before
// the stop does not carry over (FR-S5).
func TestRetryAfterFailedApproveNeedsNewApprove(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptFailed, contracts.ReasonGateFailed)
	te := newEnv(t, w)
	var mu sync.Mutex
	tries := 0
	te.RegisterActivityWithOptions(func(ctx context.Context, in activity.ApproveInput) (activity.ApproveResult, error) {
		mu.Lock()
		tries++
		first := tries == 1
		mu.Unlock()
		if first {
			return activity.ApproveResult{}, temporal.NewNonRetryableApplicationError("default branch unreadable", activity.ErrTypePermanent, nil)
		}
		return w.Approve(ctx, in)
	}, sdkRegister("Approve"))
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(te, time.Hour, command(github.CommandRetry, testCommentBase))
	var waiting Status
	te.RegisterDelayedCallback(func() { waiting = queryStatus(t, te) }, 5*time.Hour)
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equal(t, "status after /retry", waiting.Status, contracts.FeatureAwaitingApproval)
	equal(t, "approvals", w.approved, 0)
	mu.Lock()
	equal(t, "Approve tries", tries, 1)
	mu.Unlock()
}
