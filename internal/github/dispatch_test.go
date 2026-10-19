package github_test

import (
	"errors"
	"net/http"
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
