package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// TestBudgetCapRaisesPauseAlert stops on a cap, retries, and stops on the
// cap again: each stop raises its own pause alert (FR-SEC31), named by
// the feature request's revision after the move.
func TestBudgetCapRaisesPauseAlert(t *testing.T) {
	w := newWorld()
	w.run = budgetRefusal(llm.BudgetMonth)
	te := newEnv(t, w)
	signalAfter(te, time.Minute, command(github.CommandApprove, approveID))
	signalAfter(te, 2*time.Hour, command(github.CommandRetry, testCommentBase))
	signalAfter(te, 24*time.Hour, command(github.CommandCancel, cancelID))
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)

	equal(t, "alerts", w.alertCount(), 2)
	for _, in := range w.alerts {
		equal(t, "alert reason", in.Reason, "budget_cap:month")
		equal(t, "alert feature request", in.FeatureRequestID, frID)
		equal(t, "alert workflow", in.WorkflowID, featureInput().Start.WorkflowID)
	}
}

// TestOtherStopsRaiseNoAlert: a failed attempt is shown on the issue's
// status comment and raises no alert, which FR-SEC31 keeps for
// conditions not already visible elsewhere.
func TestOtherStopsRaiseNoAlert(t *testing.T) {
	w := newWorld()
	w.run = func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		return w.finish(n, contracts.AttemptFailed, contracts.ReasonGateFailed), nil
	}
	te := newEnv(t, w)
	approveThenCancel(te)
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)
	equal(t, "RaiseAlert calls", w.called("RaiseAlert"), 0)
}

// TestRaiseAlertFailureStillWaits: an alert that cannot be delivered is
// logged, and the run still waits in needs_attention for /cancel.
func TestRaiseAlertFailureStillWaits(t *testing.T) {
	w := newWorld()
	w.run = budgetRefusal(llm.BudgetFeatureRequest)
	w.fail["RaiseAlert"] = errors.New("github: unavailable")
	te := newEnv(t, w)
	var during Status
	te.RegisterDelayedCallback(func() { during = queryStatus(t, te) }, 12*time.Hour)
	approveThenCancel(te)
	te.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, te)
	equal(t, "status while waiting", during.Status, contracts.FeatureNeedsAttention)
	if w.called("RaiseAlert") < 2 {
		t.Errorf("RaiseAlert tried %d times, want retries within its bound", w.called("RaiseAlert"))
	}
	equal(t, "final status", w.fr, contracts.FeatureCancelled)
}
