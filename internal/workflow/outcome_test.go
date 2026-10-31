package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// The opening moves every executed run makes.
var (
	toApproval  = "planning>awaiting_approval"
	toExecuting = "awaiting_approval>executing"
	toCancelled = "needs_attention>cancelled"
)

// budgetRefusal records the attempt errored/budget_cap and returns the
// cap's error through the activity boundary, as RunAttempt does.
func budgetRefusal(kind llm.BudgetKind) func(context.Context, *world, int) (activity.AttemptOutcome, error) {
	return func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		w.finish(n, contracts.AttemptErrored, contracts.ReasonBudgetCap)
		return activity.AttemptOutcome{}, activity.Temporal(llm.BudgetError{Kind: kind, Detail: "spent 10 of 10"})
	}
}

// erroredThenPassed errors the first ordinal and passes the second.
func erroredThenPassed(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
	if n == 1 {
		return w.finish(n, contracts.AttemptErrored, contracts.ReasonProviderError), nil
	}
	return w.finish(n, contracts.AttemptPassed, ""), nil
}

// outcomeRows are the rows of the outcome table.
var outcomeRows = []struct {
	name         string
	run          func(context.Context, *world, int) (activity.AttemptOutcome, error)
	wantAttempts []string
	wantTask     contracts.TaskStatus
	wantMoves    []string
	wantDelivers int
}{
	{
		name: "all gates pass: delivered by the stub, which stops in needs_attention", run: returns(contracts.AttemptPassed, ""),
		wantAttempts: []string{"passed/"}, wantTask: contracts.TaskDone, wantDelivers: 1,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention error:" + activity.DetailDeliveryStub, toCancelled},
	},
	{
		name: "a gate fails", run: returns(contracts.AttemptFailed, contracts.ReasonGateFailed),
		wantAttempts: []string{"failed/gate_failed"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
	},
	{
		name: "runner step limit", run: returns(contracts.AttemptFailed, contracts.ReasonStepLimit),
		wantAttempts: []string{"failed/step_limit"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
	},
	{
		name: "runner token limit", run: returns(contracts.AttemptFailed, contracts.ReasonTokenLimit),
		wantAttempts: []string{"failed/token_limit"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
	},
	{
		name: "runner wall clock", run: returns(contracts.AttemptFailed, contracts.ReasonWallClock),
		wantAttempts: []string{"failed/wall_clock"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
	},
	{
		name: "runner repeat errors", run: returns(contracts.AttemptFailed, contracts.ReasonRepeatErrors),
		wantAttempts: []string{"failed/repeat_errors"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
	},
	{
		name: "runner invalid output", run: returns(contracts.AttemptFailed, contracts.ReasonInvalidOutput),
		wantAttempts: []string{"failed/invalid_output"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
	},
	{
		name: "errored once: a new ordinal, which passes", run: erroredThenPassed,
		wantAttempts: []string{"errored/provider_error", "passed/"}, wantTask: contracts.TaskDone, wantDelivers: 1,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention error:" + activity.DetailDeliveryStub, toCancelled},
	},
	{
		name: "errored twice: needs_attention with reason error", run: returns(contracts.AttemptErrored, contracts.ReasonSandboxError),
		wantAttempts: []string{"errored/sandbox_error", "errored/sandbox_error"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention error:attempts errored: sandbox_error", toCancelled},
	},
	{
		name: "feature request cap: budget_cap naming it, attempts kept", run: budgetRefusal(llm.BudgetFeatureRequest),
		wantAttempts: []string{"errored/budget_cap"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention budget_cap:feature_request", toCancelled},
	},
	{
		name: "monthly cap: budget_cap naming it, attempts kept", run: budgetRefusal(llm.BudgetMonth),
		wantAttempts: []string{"errored/budget_cap"}, wantTask: contracts.TaskCancelled,
		wantMoves: []string{toApproval, toExecuting, "executing>needs_attention budget_cap:month", toCancelled},
	},
}

// TestOutcomeTable runs one feature workflow per row of the outcome table
// in ticket-8-pre-plan.md, "Carried into ticket 8", and checks the
// attempt, task and feature request each row names. Every run that stops
// in needs_attention is then cancelled, which also covers /cancel there.
func TestOutcomeTable(t *testing.T) {
	for _, tt := range outcomeRows {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			w.run = tt.run
			env := newEnv(t, w)
			approveThenCancel(env)
			env.ExecuteWorkflow(FeatureWorkflow, featureInput())
			checkCompleted(t, env)
			equalSlices(t, "attempts", w.attemptStates(), tt.wantAttempts)
			equal(t, "task", w.taskStatus(), tt.wantTask)
			equalSlices(t, "feature request moves", w.moves(), tt.wantMoves)
			equal(t, "deliveries", w.delivers, tt.wantDelivers)
			equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
		})
	}
}

// TestOutcomeTaskStatusBeforeCancel checks the task status each row
// leaves before /cancel closes the run: needs_attention for a failed,
// capped or twice-errored task.
func TestOutcomeTaskStatusBeforeCancel(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *world, int) (activity.AttemptOutcome, error)
		want contracts.TaskStatus
	}{
		{"failed", returns(contracts.AttemptFailed, contracts.ReasonGateFailed), contracts.TaskNeedsAttention},
		{"errored twice", returns(contracts.AttemptErrored, contracts.ReasonSandboxError), contracts.TaskNeedsAttention},
		{"passed", returns(contracts.AttemptPassed, ""), contracts.TaskDone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			w.run = tt.run
			env := newEnv(t, w)
			signalAfter(env, time.Minute, command(github.CommandApprove, 1))
			var before contracts.TaskStatus
			env.RegisterDelayedCallback(func() { before = w.taskStatus() }, 12*time.Hour)
			signalAfter(env, 24*time.Hour, command(github.CommandCancel, 2))
			env.ExecuteWorkflow(FeatureWorkflow, featureInput())
			checkCompleted(t, env)
			equal(t, "task before cancel", before, tt.want)
		})
	}
}

// TestNonRetryableErrorOutsideAttempt routes a step that fails for good
// to needs_attention with reason error, instead of failing the workflow.
func TestNonRetryableErrorOutsideAttempt(t *testing.T) {
	permanent := temporal.NewNonRetryableApplicationError("plan 2222 is rejected", activity.ErrTypePermanent, nil)
	tests := []struct {
		name      string
		failing   string
		wantMoves []string
		wantTask  contracts.TaskStatus
	}{
		{
			name: "analyze", failing: "Analyze",
			// The feature request was never read, so there is nothing to
			// move; the workflow still waits and ends on /cancel.
			wantMoves: nil,
		},
		{
			name: "plan comment", failing: "PostPlanComment",
			wantMoves: []string{toApproval, "awaiting_approval>planning", "planning>needs_attention error:plan 2222 is rejected", toCancelled},
		},
		{
			name: "approve", failing: "Approve",
			wantMoves: []string{toApproval, "awaiting_approval>planning", "planning>needs_attention error:plan 2222 is rejected", toCancelled},
		},
		{
			name: "create task", failing: "CreateTask",
			wantMoves: []string{toApproval, toExecuting, "executing>needs_attention error:plan 2222 is rejected", toCancelled},
		},
		{
			name: "prepare attempt", failing: "PrepareAttempt", wantTask: contracts.TaskCancelled,
			wantMoves: []string{toApproval, toExecuting, "executing>needs_attention error:plan 2222 is rejected", toCancelled},
		},
		{
			name: "deliver", failing: "Deliver", wantTask: contracts.TaskDone,
			wantMoves: []string{toApproval, toExecuting, "executing>needs_attention error:plan 2222 is rejected", toCancelled},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			w.run = returns(contracts.AttemptPassed, "")
			w.fail[tt.failing] = permanent
			env := newEnv(t, w)
			approveThenCancel(env)
			env.ExecuteWorkflow(FeatureWorkflow, featureInput())
			checkCompleted(t, env)
			equalSlices(t, "feature request moves", w.moves(), tt.wantMoves)
			equal(t, "task", w.taskStatus(), tt.wantTask)
			equal(t, "calls to "+tt.failing, w.called(tt.failing), 1)
		})
	}
}

// TestAnalyzeProblem stops an issue that cannot be planned, such as one
// the owner did not write, in needs_attention with reason error and the
// problem, without posting a plan or waiting for /approve.
func TestAnalyzeProblem(t *testing.T) {
	w := newWorld()
	w.problem = "the issue was not written by the owner"
	env := newEnv(t, w)
	approveThenCancel(env)
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equalSlices(t, "moves", w.moves(), []string{"planning>needs_attention error:the issue was not written by the owner", toCancelled})
	equal(t, "plan comments", w.comments, 0)
	equal(t, "approvals", w.approved, 0)
}

// checkCompleted fails the test unless the workflow completed without
// an error.
func checkCompleted(t *testing.T, env interface {
	IsWorkflowCompleted() bool
	GetWorkflowError() error
}) {
	t.Helper()
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		var canceled *temporal.CanceledError
		if !errors.As(err, &canceled) {
			t.Fatalf("workflow error: %v", err)
		}
	}
}
