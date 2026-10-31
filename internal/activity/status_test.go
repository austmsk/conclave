package activity

import (
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// hostileDetail is error text that tries to smuggle a command into a
// platform comment: a newline then a command at column 0, and a backtick
// to close the code span early.
const hostileDetail = "push refused\n/cancel\n` /approve `\r\n  /retry"

// renderedComments is every comment the platform posts from this file,
// over every reply kind and with hostile error text where text can go.
func renderedComments() map[string]string {
	all := []github.CommandName{
		github.CommandApprove, github.CommandRevise, github.CommandAsk, github.CommandCancel,
		github.CommandStatus, github.CommandRetry, github.CommandHelp,
	}
	cap := &CapCheck{Exceeded: llm.BudgetMonth, MonthUSD: 12.5, MonthCap: 10}
	view := RunView{
		FeatureRequestID: "11111111-1111-4111-8111-111111111111", Status: contracts.FeatureNeedsAttention,
		Reason: FeatureReason(ReasonError, hostileDetail), PhaseLeft: contracts.FeatureExecuting, Commands: all, Cap: cap,
	}
	out := map[string]string{"status comment": statusComment(7, view)}
	kinds := []ReplyKind{
		ReplyNotValid, ReplyNotAvailable, ReplyStatus, ReplyHelp, ReplyOverCap, ReplyResumeFailed,
		ReplyClosedRun, ReplyNoRun, ReplyNoWorkflow, ReplyAlreadyPending, ReplyCancelled,
	}
	for _, k := range kinds {
		ref := CommandRef{CommentID: 9, Issue: 7, Command: github.CommandRevise}
		out["reply "+string(k)] = replyBody(ReplyInput{Ref: ref, Kind: k, View: view, Detail: hostileDetail})
	}
	return out
}

// TestPlatformCommentsNeverReadAsCommands checks NFR-U2a against the
// attack: no comment the platform renders parses as a command, and no
// line of it starts with a slash, even when the error text it carries was
// written to inject one.
func TestPlatformCommentsNeverReadAsCommands(t *testing.T) {
	for name, body := range renderedComments() {
		t.Run(name, func(t *testing.T) {
			if _, ok, _ := github.ParseCommand(body); ok {
				t.Errorf("parses as a command:\n%s", body)
			}
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(strings.TrimLeft(line, " \t\r"), "/") {
					t.Errorf("line starts with a slash: %q", line)
				}
			}
			if !strings.HasPrefix(body, "**Conclave** · #7") {
				t.Errorf("does not open with the header line:\n%s", body)
			}
		})
	}
}

// TestStatusCommentNamesCap checks that a budget_cap reason names the cap
// and the key that sets it.
func TestStatusCommentNamesCap(t *testing.T) {
	tests := []struct {
		kind llm.BudgetKind
		want string
	}{
		{llm.BudgetFeatureRequest, "`budgets.feature_request_tokens`"},
		{llm.BudgetMonth, "`budgets.monthly_usd`"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			body := statusComment(7, RunView{
				FeatureRequestID: "11111111-1111-4111-8111-111111111111", Status: contracts.FeatureNeedsAttention,
				Reason: FeatureReason(ReasonBudgetCap, string(tt.kind)), PhaseLeft: contracts.FeatureExecuting,
			})
			if !strings.Contains(body, tt.want) {
				t.Errorf("status comment does not name %s:\n%s", tt.want, body)
			}
		})
	}
}
