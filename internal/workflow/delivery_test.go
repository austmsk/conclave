package workflow

import (
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
)

// pendingCI is a read of CI with a required check still running.
var pendingCI = activity.ChecksResult{Detail: "waiting for build"}

// checksPendingUntil makes reads 1 to n find CI pending and every later
// read find it passed.
func checksPendingUntil(n int) func(int) activity.ChecksResult {
	return func(read int) activity.ChecksResult {
		if read <= n {
			return pendingCI
		}
		return activity.ChecksResult{Completed: true, Passed: true}
	}
}

// deliveredOnce checks a run delivered with every delivery effect once:
// one pr_writer call, one landing, one pull request, one ready mark.
func deliveredOnce(t *testing.T, w *world) {
	t.Helper()
	d := w.delivery()
	equal(t, "pr_writer calls", d.modelCalls, 1)
	equal(t, "landings", d.landings, 1)
	equal(t, "pull requests opened", d.opens, 1)
	equal(t, "drafts marked ready", d.readies, 1)
	equal(t, "stub deliveries", w.delivers, 0)
}

// TestDeliveryEndsDelivered runs a passed task through the whole delivery
// sequence: the pull request is written, landed, rebased and opened, CI
// passes, the draft is marked ready, and the feature request moves from
// executing to delivered, the only move into delivered (FR-A7). The
// workflow then ends with no /cancel.
func TestDeliveryEndsDelivered(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	env := newEnv(t, w)
	signalAfter(env, time.Minute, command(github.CommandApprove, approveID))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, toDelivered})
	deliveredOnce(t, w)
	for _, name := range []string{"WritePullRequest", "Land", "Finalize", "OpenPullRequest", "ReadChecks", "MarkReady"} {
		equal(t, "calls to "+name, w.called(name), 1)
	}
	equal(t, "task", w.taskStatus(), contracts.TaskDone)
	equal(t, "status comment", w.status.Status, contracts.FeatureDelivered)
}

// TestDeliveryStopThenRetry stops delivery at each step in turn, then
// sends /retry: the feature request stops in needs_attention with the
// step's reason, having left executing, and /retry re-enters the delivery
// sequence at its start and ends delivered, with no delivery effect
// repeated.
func TestDeliveryStopThenRetry(t *testing.T) {
	permanent := temporal.NewNonRetryableApplicationError("github refused", activity.ErrTypePermanent, nil)
	stopError := "executing>needs_attention error:github refused"
	tests := []struct {
		name     string
		setup    func(w *world)
		wantStop string
	}{
		{"write pull request fails", func(w *world) { w.failOnce["WritePullRequest"] = permanent }, stopError},
		{"land fails", func(w *world) { w.failOnce["Land"] = permanent }, stopError},
		{"rebased artifact fails a gate", func(w *world) {
			w.stops["Finalize"] = []activity.DeliveryStep{{Stop: activity.ReasonRebaseGateFailed, Detail: "tests failed"}}
		}, "executing>needs_attention rebase_gate_failed:tests failed"},
		{"open pull request fails", func(w *world) { w.failOnce["OpenPullRequest"] = permanent }, stopError},
		{"CI fails", failCIOnce, stopCIFailed},
		{"CI times out", func(w *world) {
			w.ci = activity.CIWait{PollEvery: 10 * time.Minute, Timeout: 30 * time.Minute}
			w.checks = checksPendingUntil(4)
		}, "executing>needs_attention ci_timeout:waiting for build"},
		{"mark ready fails", func(w *world) { w.failOnce["MarkReady"] = permanent }, stopError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			w.run = returns(contracts.AttemptPassed, "")
			tt.setup(w)
			env := newEnv(t, w)
			signalAfter(env, time.Minute, command(github.CommandApprove, approveID))
			signalAfter(env, 3*time.Hour, command(github.CommandRetry, testCommentBase))
			env.ExecuteWorkflow(FeatureWorkflow, featureInput())
			checkCompleted(t, env)
			equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, tt.wantStop, "needs_attention>executing", toDelivered})
			deliveredOnce(t, w)
			equal(t, "RunAttempt calls", w.called("RunAttempt"), 1)
			wantAudit(t, w, testCommentBase, contracts.FeatureNeedsAttention, activity.CommandAccepted, "")
		})
	}
}

// TestRebaseConflictRetryStopsAgain: the plan's base is pinned
// write-once, so a /retry after a rebase conflict meets the same conflict
// and stops again with the same reason, opening no pull request and
// pushing nothing more; /cancel is the way out until Milestone 4
// (decision 0014).
func TestRebaseConflictRetryStopsAgain(t *testing.T) {
	conflict := activity.DeliveryStep{Stop: activity.ReasonRebaseConflict, Detail: "src/tally.go"}
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	w.stops["Finalize"] = []activity.DeliveryStep{conflict, conflict}
	env := newEnv(t, w)
	signalAfter(env, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(env, 2*time.Hour, command(github.CommandRetry, testCommentBase))
	signalAfter(env, 24*time.Hour, command(github.CommandCancel, cancelID))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	stop := "executing>needs_attention rebase_conflict:src/tally.go"
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, stop, "needs_attention>executing", stop, toCancelled})
	d := w.delivery()
	equal(t, "pr_writer calls", d.modelCalls, 1)
	equal(t, "landings", d.landings, 1)
	equal(t, "pull requests opened", d.opens, 0)
	equal(t, "OpenPullRequest calls", w.called("OpenPullRequest"), 0)
	equal(t, "Finalize calls", w.called("Finalize"), 2)
}

// TestCancelDuringCIWait sends /cancel while the workflow waits between
// reads of CI: the wait ends at once, nothing is marked ready, and the
// feature request moves from executing to cancelled.
func TestCancelDuringCIWait(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	w.ci = activity.CIWait{PollEvery: time.Hour, Timeout: 24 * time.Hour}
	w.checks = checksPendingUntil(1000)
	env := newEnv(t, w)
	signalAfter(env, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(env, 90*time.Minute, command(github.CommandCancel, cancelID))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, "executing>cancelled"})
	equal(t, "CI reads", w.delivery().reads, 2)
	equal(t, "MarkReady calls", w.called("MarkReady"), 0)
	equal(t, "pull requests opened", w.delivery().opens, 1)
}

// TestCIPollsUntilTimeout reads CI every PollEvery and stops at Timeout:
// with a ten-minute poll and a thirty-minute timeout, reads at 0, 10, 20
// and 30 minutes find CI pending, and the stop lands at 30 minutes.
func TestCIPollsUntilTimeout(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	w.ci = activity.CIWait{PollEvery: 10 * time.Minute, Timeout: 30 * time.Minute}
	w.checks = checksPendingUntil(1000)
	env := newEnv(t, w)
	signalAfter(env, time.Minute, command(github.CommandApprove, approveID))
	var before, after Status
	// Approval at one minute opens the pull request at once, in test time.
	env.RegisterDelayedCallback(func() { before = queryStatus(t, env) }, 30*time.Minute)
	env.RegisterDelayedCallback(func() { after = queryStatus(t, env) }, 32*time.Minute)
	signalAfter(env, 24*time.Hour, command(github.CommandCancel, cancelID))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "status before the timeout", before.Status, contracts.FeatureExecuting)
	equal(t, "status after the timeout", after.Status, contracts.FeatureNeedsAttention)
	equal(t, "reason after the timeout", after.Reason, "ci_timeout:waiting for build")
	equal(t, "CI reads", w.delivery().reads, 4)
	equal(t, "MarkReady calls", w.called("MarkReady"), 0)
}

// TestCIPassesAfterPolling reads pending CI twice and passed on the third
// read, then delivers.
func TestCIPassesAfterPolling(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	w.checks = checksPendingUntil(2)
	env := newEnv(t, w)
	signalAfter(env, time.Minute, command(github.CommandApprove, approveID))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "CI reads", w.delivery().reads, 3)
	equalSlices(t, "moves", w.moves(), []string{toApproval, toExecuting, toDelivered})
	deliveredOnce(t, w)
}

// TestDeliveryBeforeTicket9KeepsStub runs a workflow that reads
// DefaultVersion for the delivery change, as a run started before ticket
// 9 replays: it calls the ticket 8 stub and stops in needs_attention,
// with no delivery activity called, so its history still replays.
func TestDeliveryBeforeTicket9KeepsStub(t *testing.T) {
	w := newWorld()
	w.run = returns(contracts.AttemptPassed, "")
	env := newEnv(t, w)
	env.OnGetVersion(versionDelivery, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "moves", w.moves(), []string{
		toApproval, toExecuting, "executing>needs_attention error:" + activity.DetailDeliveryStub, toCancelled,
	})
	equal(t, "stub deliveries", w.delivers, 1)
	for _, name := range []string{"WritePullRequest", "Land", "Finalize", "OpenPullRequest", "ReadChecks", "MarkReady"} {
		equal(t, "calls to "+name, w.called(name), 0)
	}
}
