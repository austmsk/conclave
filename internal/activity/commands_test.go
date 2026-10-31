package activity

import (
	"fmt"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// commandRef is a command on the fixture's issue. Comment IDs are unique
// per fixture, because every fixture has its own issue number and the
// tests share one database.
func (f *fixture) commandRef(name github.CommandName, n int) CommandRef {
	return CommandRef{
		CommentID: int64(f.issue)*1000 + int64(n), DeliveryID: "delivery-" + fmt.Sprint(n),
		WorkflowID: f.workflowID(), Repo: f.repoRef(), Issue: f.issue, Command: name, ActorID: testOwner,
	}
}

// commandRows counts the audit rows for a comment ID.
func (f *fixture) commandRows(commentID int64) int {
	return f.count(`SELECT count(*) FROM commands WHERE comment_id = $1`, commentID)
}

// TestRecordCommandTwice records one command twice, the second time with
// a different decision, as a retry after a lost result or a second
// delivery would: one row, holding the first decision.
func TestRecordCommandTwice(t *testing.T) {
	f := newFixture(t)
	res := f.analyzed()
	ref := f.commandRef(github.CommandStatus, 1)
	in := RecordCommandInput{
		Ref: ref, FeatureRequestID: res.FeatureRequestID, Status: contracts.FeaturePlanning,
		Outcome: CommandRejected, Reason: RejectNotValid,
	}
	f.must(f.a.RecordCommand(f.ctx, in))
	in.Outcome, in.Reason = CommandAccepted, ""
	f.must(f.a.RecordCommand(f.ctx, in))
	equalInt(t, "audit rows", f.commandRows(ref.CommentID), 1)
	var outcome, reason, actor, status string
	f.must(ownerPool.QueryRow(f.ctx, `SELECT outcome, reason, actor, status_at_decision FROM commands WHERE comment_id = $1`,
		ref.CommentID).Scan(&outcome, &reason, &actor, &status))
	equalStr(t, "outcome", outcome, CommandRejected)
	equalStr(t, "reason", reason, RejectNotValid)
	equalStr(t, "actor", actor, store.ActorGitHub(testOwner))
	equalStr(t, "status", status, string(contracts.FeaturePlanning))
}

// TestRecordCommandBeforeFeatureRequest records a command that arrived
// before analysis recorded the feature request: no feature request and no
// status, still one row.
func TestRecordCommandBeforeFeatureRequest(t *testing.T) {
	f := newFixture(t)
	ref := f.commandRef(github.CommandHelp, 1)
	f.must(f.a.RecordCommand(f.ctx, RecordCommandInput{Ref: ref, Outcome: CommandRejected, Reason: RejectNoRun}))
	equalInt(t, "audit rows", f.commandRows(ref.CommentID), 1)
}

// TestRecordCommandRefusesBadRecord refuses, permanently, rows the audit
// must not hold: an accepted command with a reason, a rejected one
// without, an unknown command word.
func TestRecordCommandRefusesBadRecord(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name string
		in   RecordCommandInput
	}{
		{"accepted with a reason", RecordCommandInput{Ref: f.commandRef(github.CommandHelp, 1), Outcome: CommandAccepted, Reason: "x"}},
		{"rejected without a reason", RecordCommandInput{Ref: f.commandRef(github.CommandHelp, 2), Outcome: CommandRejected}},
		{"unknown command", RecordCommandInput{Ref: f.commandRef("resume", 3), Outcome: CommandRejected, Reason: RejectNotValid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePermanent(t, f.a.RecordCommand(f.ctx, tt.in))
			equalInt(t, "audit rows", f.commandRows(tt.in.Ref.CommentID), 0)
		})
	}
}

// TestUpsertStatusCommentTwice posts the status comment and then updates
// it, once with the first post's reply lost: the issue holds one comment,
// carrying the last status.
func TestUpsertStatusCommentTwice(t *testing.T) {
	f := newFixture(t)
	res := f.analyzed()
	in := StatusCommentInput{Repo: f.repoRef(), Issue: f.issue, View: RunView{FeatureRequestID: res.FeatureRequestID, Status: contracts.FeaturePlanning}}
	f.gh.lostReplies = 1
	if err := f.a.UpsertStatusComment(f.ctx, in); err == nil {
		t.Fatal("the lost reply was not reported")
	}
	f.must(f.a.UpsertStatusComment(f.ctx, in))
	in.View = RunView{FeatureRequestID: res.FeatureRequestID, Status: contracts.FeatureNeedsAttention, Reason: "budget_cap:month",
		PhaseLeft: contracts.FeatureExecuting, Commands: []github.CommandName{github.CommandRetry, github.CommandCancel}}
	f.must(f.a.UpsertStatusComment(f.ctx, in))

	equalInt(t, "comments created", f.gh.creates, 1)
	equalInt(t, "edits", f.gh.edits, 2)
	body := f.gh.comments[0].Body
	for _, want := range []string{"`needs_attention`", "monthly spending cap", "`executing`", "`/retry`, `/cancel`", statusMarker(res.FeatureRequestID)} {
		if !strings.Contains(body, want) {
			t.Errorf("status comment lacks %q:\n%s", want, body)
		}
	}
}

// TestReplyToCommandTwice replies to one command twice: one comment.
func TestReplyToCommandTwice(t *testing.T) {
	f := newFixture(t)
	in := ReplyInput{Ref: f.commandRef(github.CommandApprove, 1), Kind: ReplyNotValid, View: RunView{Status: contracts.FeatureExecuting}}
	f.must(f.a.ReplyToCommand(f.ctx, in))
	f.must(f.a.ReplyToCommand(f.ctx, in))
	equalInt(t, "comments created", f.gh.creates, 1)
	if !strings.Contains(f.gh.comments[0].Body, "`/approve` is not valid while this run is `executing`") {
		t.Errorf("reply = %q", f.gh.comments[0].Body)
	}
}

// TestCheckCaps compares the feature request's spend with each cap: under
// it passes, at or over it names the cap, and the feature request cap is
// named before the monthly one.
func TestCheckCaps(t *testing.T) {
	f := newFixture(t)
	res := f.analyzed()
	f.costEvent(res.FeatureRequestID, "", 100, 0.5)
	base, err := f.a.ledger.Spend(f.ctx, "", res.FeatureRequestID)
	f.must(err)
	equalInt(t, "tokens", base.FeatureRequestTokens, 100)
	month := base.MonthUSD
	tests := []struct {
		name     string
		frCap    int
		monthCap float64
		want     llm.BudgetKind
	}{
		{"under both", 101, month + 1, ""},
		{"at the feature request cap", 100, month + 1, llm.BudgetFeatureRequest},
		{"over the feature request cap", 99, month + 1, llm.BudgetFeatureRequest},
		{"at the monthly cap", 1000, month, llm.BudgetMonth},
		{"over the monthly cap", 1000, month - 0.25, llm.BudgetMonth},
		{"over both", 99, month - 0.25, llm.BudgetFeatureRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.a.d.RouterConfig.Budgets = llm.Budgets{FeatureRequestTokens: tt.frCap, MonthlyUSD: tt.monthCap}
			got, err := f.a.CheckCaps(f.ctx, res.FeatureRequestID)
			f.must(err)
			equalStr(t, "exceeded", string(got.Exceeded), string(tt.want))
			equalInt(t, "tokens reported", got.FeatureRequestTokens, 100)
			equalInt(t, "cap reported", got.FeatureRequestCap, tt.frCap)
		})
	}
}

// costEvent records spend as the ledger does, as the owner.
func (f *fixture) costEvent(featureRequestID, attemptID string, tokens int, usd float64) {
	f.t.Helper()
	var attempt any
	if attemptID != "" {
		attempt = attemptID
	}
	_, err := ownerPool.Exec(f.ctx, `INSERT INTO cost_events
		(idempotency_key, repo_id, attempt_id, feature_request_id, role, provider, model, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, $3, $4, 'implementer', 'anthropic', 'fake-model', $5, 0, $6)`,
		fmt.Sprintf("test-%d-%s-%d", f.issue, attemptID, tokens), repoID, attempt, featureRequestID, tokens, usd)
	f.must(err)
}

// stoppedTask takes a fresh issue to a task in needs_attention whose
// first attempt errored on a cap, with its spend recorded.
func (f *fixture) stoppedTask() (TaskRef, AttemptRef) {
	f.t.Helper()
	task, ref, _ := f.attempt()
	f.costEvent(f.featureOf(ref.AttemptID), ref.AttemptID, 500, 0.01)
	_, err := f.a.SettleAttempt(f.ctx, SettleInput{AttemptID: ref.AttemptID, Reason: contracts.ReasonBudgetCap, Actor: "workflow:test"})
	f.must(err)
	_, err = f.a.Transition(f.ctx, TransitionInput{
		Kind: contracts.EntityTask, ID: task.TaskID, From: string(contracts.TaskRunning),
		To: string(contracts.TaskNeedsAttention), Revision: task.Revision, Reason: string(contracts.ReasonBudgetCap), Actor: "workflow:test",
	})
	f.must(err)
	return task, ref
}

// TestResumeTaskTwice reopens a stopped task twice: one transition, the
// same next ordinal both times, and the first attempt and its cost as
// they were.
func TestResumeTaskTwice(t *testing.T) {
	f := newFixture(t)
	task, ref := f.stoppedTask()
	in := ResumeTaskInput{TaskID: task.TaskID, Actor: "workflow:test"}
	first, err := f.a.ResumeTask(f.ctx, in)
	f.must(err)
	second, err := f.a.ResumeTask(f.ctx, in)
	f.must(err)
	equalInt(t, "first ordinal", first.Ordinal, 2)
	equalInt(t, "second ordinal", second.Ordinal, 2)
	now, err := f.a.LoadTask(f.ctx, task.TaskID)
	f.must(err)
	equalStr(t, "task", string(now.Status), string(contracts.TaskRunning))
	equalInt(t, "task audit rows", f.transitions(task.TaskID), 2)
	row, err := readAttempt(f.ctx, appPool, ref.AttemptID)
	f.must(err)
	if row.State != contracts.AttemptErrored || row.Reason != contracts.ReasonBudgetCap {
		t.Errorf("attempt 1 = %s/%s, want errored/budget_cap kept", row.State, row.Reason)
	}
	equalInt(t, "attempt 1 cost events", f.count(`SELECT count(*) FROM cost_events WHERE attempt_id = $1`, ref.AttemptID), 1)
	// The next attempt can now be prepared at the ordinal returned.
	next, err := f.a.PrepareAttempt(f.ctx, PrepareAttemptInput{TaskID: task.TaskID, Ordinal: first.Ordinal, Role: contracts.RoleImplementer})
	f.must(err)
	equalInt(t, "next ordinal", next.Ordinal, 2)
}

// TestResumeTaskRefuses will not reopen a task with an attempt still
// running, or one already finished.
func TestResumeTaskRefuses(t *testing.T) {
	t.Run("attempt still running", func(t *testing.T) {
		f := newFixture(t)
		task, _, _ := f.attempt()
		_, err := f.a.ResumeTask(f.ctx, ResumeTaskInput{TaskID: task.TaskID, Actor: "workflow:test"})
		requirePermanent(t, err)
	})
	t.Run("task cancelled", func(t *testing.T) {
		f := newFixture(t)
		task, _ := f.stoppedTask()
		f.must(f.a.CancelTask(f.ctx, CancelTaskInput{TaskID: task.TaskID, Actor: "workflow:test"}))
		_, err := f.a.ResumeTask(f.ctx, ResumeTaskInput{TaskID: task.TaskID, Actor: "workflow:test"})
		requirePermanent(t, err)
	})
}

// TestLookupFeature finds nothing for an issue never analyzed, the
// feature request and its status for one that was, and reports a command
// already in the audit.
func TestLookupFeature(t *testing.T) {
	f := newFixture(t)
	ref := f.commandRef(github.CommandRetry, 1)
	in := LookupFeatureInput{WorkflowID: f.workflowID(), CommentID: ref.CommentID}
	none, err := f.a.LookupFeature(f.ctx, in)
	f.must(err)
	if none.Found || none.Recorded {
		t.Fatalf("before analysis = %+v, want nothing", none)
	}
	res := f.analyzed()
	_, err = f.a.Transition(f.ctx, TransitionInput{
		Kind: contracts.EntityFeatureRequest, ID: res.FeatureRequestID, From: string(contracts.FeaturePlanning),
		To: string(contracts.FeatureCancelled), Revision: res.Revision, Actor: "workflow:test",
	})
	f.must(err)
	f.must(f.a.RecordCommand(f.ctx, RecordCommandInput{Ref: ref, FeatureRequestID: res.FeatureRequestID,
		Status: contracts.FeatureCancelled, Outcome: CommandRejected, Reason: RejectClosedRun}))
	got, err := f.a.LookupFeature(f.ctx, in)
	f.must(err)
	if !got.Found || got.FeatureRequestID != res.FeatureRequestID || got.Status != contracts.FeatureCancelled || !got.Recorded {
		t.Errorf("after analysis = %+v, want the cancelled feature request and the command recorded", got)
	}
}
