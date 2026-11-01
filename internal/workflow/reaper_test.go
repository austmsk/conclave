package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/activity"
)

// TestReaperWorkflowSweeps runs one scheduled sweep: the cutoff reaches
// the activity, and its result is the workflow's.
func TestReaperWorkflowSweeps(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ReaperWorkflow)
	var got time.Duration
	env.RegisterActivityWithOptions(func(_ context.Context, in activity.SweepInput) (activity.SweepResult, error) {
		got = in.OlderThan
		return activity.SweepResult{Released: 1, Reaped: 2}, nil
	}, sdkactivity.RegisterOptions{Name: "SweepOrphans"})
	env.ExecuteWorkflow(ReaperWorkflow, ReaperInput{OlderThan: 2 * time.Hour})
	checkCompleted(t, env)
	var res activity.SweepResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	equal(t, "cutoff", got, 2*time.Hour)
	equal(t, "result", res, activity.SweepResult{Released: 1, Reaped: 2})
}

// TestReaperWorkflowRetriesThenFails: a sweep that keeps failing is tried
// three times and fails the run; the next scheduled run is the retry.
func TestReaperWorkflowRetriesThenFails(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ReaperWorkflow)
	tries := 0
	env.RegisterActivityWithOptions(func(context.Context, activity.SweepInput) (activity.SweepResult, error) {
		tries++
		return activity.SweepResult{}, errors.New("docker: unavailable")
	}, sdkactivity.RegisterOptions{Name: "SweepOrphans"})
	env.ExecuteWorkflow(ReaperWorkflow, ReaperInput{OlderThan: 2 * time.Hour})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() == nil {
		t.Fatal("a failing sweep did not fail the run")
	}
	equal(t, "tries", tries, 3)
}
