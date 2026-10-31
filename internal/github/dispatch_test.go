package github_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

// comment returns an owner's comment event with its own comment ID.
func comment(body string, id int64) []byte {
	e := githubtest.CommentEvent(body)
	e.CommentID = id
	return e.JSON()
}

func TestCommandsDispatchedInOrder(t *testing.T) {
	h := newHarness(t, options{})
	h.send(t, "issues", githubtest.LabeledEvent().JSON())
	want := []github.CommandName{github.CommandApprove, github.CommandStatus, github.CommandCancel}
	for i, name := range want {
		wantStatus(t, h.send(t, "issue_comment", comment("/"+string(name), int64(100+i))), http.StatusAccepted)
	}
	h.settle(t)

	calls := h.starter.Calls()
	if len(calls) != 1+len(want) || calls[0].Start == nil {
		t.Fatalf("calls %+v, want a start then %d signals", calls, len(want))
	}
	for i, name := range want {
		c := calls[i+1]
		if c.Signal == nil || c.Signal.Command.Name != name || c.Err != nil {
			t.Fatalf("call %d: %+v, want signal %s delivered", i+1, c, name)
		}
	}
}

func TestDispatcherDrainsOnClose(t *testing.T) {
	h := newHarness(t, options{noRun: true})
	h.send(t, "issues", githubtest.LabeledEvent().JSON())
	h.send(t, "issue_comment", comment("/approve", 100))
	h.send(t, "issue_comment", comment("/status", 101))

	h.start(t)
	h.settle(t)

	if n := len(h.starter.Calls()); n != 3 {
		t.Fatalf("starter got %d calls before Close returned, want all 3 queued", n)
	}
	if n := len(h.records(t, "level", "WARN")); n != 0 {
		t.Errorf("drained jobs were logged as dropped:\n%s", h.logs.String())
	}
}

func TestDispatcherDropsAfterDrainDeadline(t *testing.T) {
	starter := &githubtest.FakeStarter{Block: make(chan struct{})}
	h := newHarness(t, options{noRun: true, starter: starter})
	h.sendAs(t, "dddd-0001", "issues", githubtest.LabeledEvent().JSON())
	h.sendAs(t, "dddd-0002", "issue_comment", comment("/approve", 100))
	h.sendAs(t, "dddd-0003", "issue_comment", comment("/status", 101))
	h.start(t)

	h.disp.Close(100 * time.Millisecond)
	// Run may take one job after the deadline and log its drop itself.
	select {
	case <-h.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the drain deadline")
	}

	for _, delivery := range []string{"dddd-0002", "dddd-0003"} {
		recs := h.records(t, "delivery", delivery)
		if len(recs) != 1 || recs[0]["level"] != "WARN" {
			t.Errorf("delivery %s: want one WARN drop line:\n%s", delivery, h.logs.String())
		}
	}
	if n := len(starter.Calls()); n != 0 {
		t.Errorf("starter answered %d calls while blocked", n)
	}
}

func TestEnqueueAfterCloseRefused(t *testing.T) {
	h := newHarness(t, options{})
	h.settle(t)

	wantStatus(t, h.send(t, "issues", githubtest.LabeledEvent().JSON()), http.StatusServiceUnavailable)

	if n := len(h.starter.Calls()); n != 0 {
		t.Fatalf("starter got %d calls after Close", n)
	}
}

func TestStarterErrorLoggedNotRetried(t *testing.T) {
	starter := &githubtest.FakeStarter{Err: errors.New("engine unavailable")}
	h := newHarness(t, options{starter: starter})

	wantStatus(t, h.send(t, "issues", githubtest.LabeledEvent().JSON()), http.StatusAccepted)
	h.settle(t)

	if n := len(starter.Calls()); n != 1 {
		t.Fatalf("starter got %d calls, want 1 and no retry", n)
	}
	recs := h.records(t, "level", "ERROR")
	if len(recs) != 1 || recs[0]["delivery"] != githubtest.FixtureDeliveryID || recs[0]["workflow_id"] != fixtureWorkflowID {
		t.Fatalf("want one ERROR line naming the delivery and workflow:\n%s", h.logs.String())
	}
}

func TestDispatcherDoesNotLeak(t *testing.T) {
	tests := []struct {
		name  string
		block bool
	}{
		{"idle", false},
		{"starter blocked past the drain deadline", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			starter := &githubtest.FakeStarter{}
			if tt.block {
				starter.Block = make(chan struct{})
			}
			h := newHarness(t, options{starter: starter})
			h.send(t, "issues", githubtest.LabeledEvent().JSON())

			h.disp.Close(50 * time.Millisecond)

			select {
			case <-h.ran:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return after Close")
			}
		})
	}
}

// panickingStarter panics on every start and passes commands to a
// FakeStarter.
type panickingStarter struct{ *githubtest.FakeStarter }

func (panickingStarter) StartFeature(context.Context, github.FeatureStart) error {
	panic("engine client misconfigured")
}

func TestDispatcherSurvivesStarterPanic(t *testing.T) {
	fake := &githubtest.FakeStarter{}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	disp := github.NewDispatcher(panickingStarter{fake}, log, 4)
	recv, err := github.NewReceiver(githubtest.WebhookConfig(), githubtest.EnabledRepos(), []byte(secret), disp, log)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	go func() { defer close(ran); disp.Run(t.Context()) }()

	for _, d := range []struct {
		event string
		body  []byte
	}{
		{"issues", githubtest.LabeledEvent().JSON()},
		{"issue_comment", githubtest.CommentEvent("/status").JSON()},
	} {
		rec := httptest.NewRecorder()
		recv.ServeHTTP(rec, githubtest.Delivery(t, secret, d.event, d.body))
		wantStatus(t, rec, http.StatusAccepted)
	}
	disp.Close(5 * time.Second)
	<-ran

	// The start never happened, so the command finds no workflow and is
	// handed to the closed-run workflow.
	calls := fake.Calls()
	if len(calls) != 2 || calls[0].Signal == nil || calls[1].Closed == nil {
		t.Fatalf("after a panicking start the command was not dispatched: %+v", calls)
	}
	if out := logs.String(); !strings.Contains(out, "starter panicked") || strings.Contains(out, "misconfigured") {
		t.Fatalf("want the panic logged as a fixed error, without its value:\n%s", out)
	}
}

// TestNoWorkflowStartsClosedCommand sends commands to an issue with no
// running feature workflow, as for a run that is delivered or cancelled,
// or never started: the dispatcher cannot tell which, so each is handed
// to the closed-run workflow, under an ID derived from the comment. The
// workflow answers by status; TestClosedCommandWorkflow in
// internal/workflow covers delivered and cancelled. A redelivery of the
// same comment maps to the same ID and is refused as a duplicate, so it
// is answered once.
func TestNoWorkflowStartsClosedCommand(t *testing.T) {
	h := newHarness(t, options{})
	h.sendAs(t, "eeee-0001", "issue_comment", comment("/retry", 300))
	h.sendAs(t, "eeee-0002", "issue_comment", comment("/retry", 300))
	h.sendAs(t, "eeee-0003", "issue_comment", comment("/status", 301))
	h.settle(t)

	var closed []githubtest.Call
	for _, c := range h.starter.Calls() {
		if c.Closed != nil {
			closed = append(closed, c)
		}
	}
	if len(closed) != 3 {
		t.Fatalf("closed-command starts = %+v, want one per delivery", closed)
	}
	e := githubtest.CommentEvent("")
	first := github.ClosedCommandWorkflowID(e.RepoID, e.IssueNumber, 300)
	tests := []struct {
		delivery string
		id       string
		err      error
		outcome  string
	}{
		{"eeee-0001", first, nil, "closed_run"},
		{"eeee-0002", first, github.ErrAlreadyStarted, "duplicate"},
		{"eeee-0003", github.ClosedCommandWorkflowID(e.RepoID, e.IssueNumber, 301), nil, "closed_run"},
	}
	for i, tt := range tests {
		c := closed[i]
		if c.WorkflowID != tt.id || !errors.Is(c.Err, tt.err) || c.Closed.DeliveryID != tt.delivery {
			t.Errorf("start %d = %s %v (delivery %s), want %s %v (delivery %s)", i, c.WorkflowID, c.Err, c.Closed.DeliveryID, tt.id, tt.err, tt.delivery)
		}
		recs := h.records(t, "delivery", tt.delivery)
		if len(recs) != 1 || recs[0]["outcome"] != tt.outcome || recs[0]["workflow_id"] != tt.id {
			t.Errorf("delivery %s logged %v, want outcome %s for %s", tt.delivery, recs, tt.outcome, tt.id)
		}
	}
}
