package activity

import (
	"errors"
	"os"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
)

func (f *fixture) capAlertInput(rev int) AlertInput {
	f.t.Helper()
	res := f.analyzed()
	return AlertInput{FeatureRequestID: res.FeatureRequestID, WorkflowID: f.workflowID(), Reason: "budget_cap:month", Revision: rev}
}

// TestRaiseAlertTwice runs the same stop's alert twice: one issue, no
// comment, and the alert in the local log.
func TestRaiseAlertTwice(t *testing.T) {
	f := newFixture(t)
	in := f.capAlertInput(3)
	f.must(f.a.RaiseAlert(f.ctx, in))
	f.must(f.a.RaiseAlert(f.ctx, in))
	issues, comments := f.alerts.counts()
	equalInt(t, "issues", issues, 1)
	equalInt(t, "comments", comments, 0)
	body := f.alerts.issues[0].Body
	for _, want := range []string{"`" + testRepo + "` issue", in.FeatureRequestID, "month cap", "/retry"} {
		if !strings.Contains(body, want) {
			t.Errorf("alert body lacks %q:\n%s", want, body)
		}
	}
	raw, err := os.ReadFile(f.alertLog)
	f.must(err)
	if n := strings.Count(string(raw), `"type":"alert"`); n != 2 {
		t.Fatalf("%d alert entries in the log, want one per try", n)
	}
}

// TestRaiseAlertSecondStopComments: the same feature request stopped
// again after /retry comments on its open issue.
func TestRaiseAlertSecondStopComments(t *testing.T) {
	f := newFixture(t)
	in := f.capAlertInput(3)
	f.must(f.a.RaiseAlert(f.ctx, in))
	in.Revision = 7
	f.must(f.a.RaiseAlert(f.ctx, in))
	issues, comments := f.alerts.counts()
	equalInt(t, "issues", issues, 1)
	equalInt(t, "comments", comments, 1)
}

// TestRaiseAlertDeliveryFailureRetries: a GitHub failure is retryable, and
// the alert is in the local log already.
func TestRaiseAlertDeliveryFailureRetries(t *testing.T) {
	f := newFixture(t)
	f.alerts.fail = errors.New("github: unavailable")
	err := f.a.RaiseAlert(f.ctx, f.capAlertInput(3))
	var app *temporal.ApplicationError
	if err == nil || (errors.As(err, &app) && app.NonRetryable()) {
		t.Fatalf("got %v, want a retryable error", err)
	}
	raw, rerr := os.ReadFile(f.alertLog)
	f.must(rerr)
	if !strings.Contains(string(raw), `"outcome":"failed"`) {
		t.Fatalf("log does not record the failed delivery:\n%s", raw)
	}
}

func TestRaiseAlertRefuses(t *testing.T) {
	f := newFixture(t)
	good := f.capAlertInput(3)
	tests := map[string]AlertInput{
		"not a cap":       {FeatureRequestID: good.FeatureRequestID, Reason: "attempt_failed", Revision: 3},
		"bad ID":          {FeatureRequestID: "x", Reason: "budget_cap:month", Revision: 3},
		"unknown request": {FeatureRequestID: "00000000-0000-4000-8000-000000000000", Reason: "budget_cap", Revision: 3},
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			err := f.a.RaiseAlert(f.ctx, in)
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || !app.NonRetryable() {
				t.Fatalf("got %v, want a permanent refusal", err)
			}
		})
	}
	if issues, _ := f.alerts.counts(); issues != 0 {
		t.Fatal("a refused alert was delivered")
	}
}

func TestIsBudgetCapReason(t *testing.T) {
	for reason, want := range map[string]bool{
		"budget_cap": true, "budget_cap:month": true, "budget_cap_x": false, "attempt_failed": false, "error:budget_cap": false,
	} {
		if got := IsBudgetCapReason(reason); got != want {
			t.Errorf("IsBudgetCapReason(%q) = %v", reason, got)
		}
	}
}
