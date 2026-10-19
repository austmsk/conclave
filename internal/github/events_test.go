package github_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

var fixtureWorkflowID = github.FeatureWorkflowID(githubtest.FixtureRepoID, 7)

// onlyCall settles h and returns the starter's single call.
func onlyCall(t *testing.T, h *harness) githubtest.Call {
	t.Helper()
	h.settle(t)
	calls := h.starter.Calls()
	if len(calls) != 1 {
		t.Fatalf("starter got %d calls, want 1", len(calls))
	}
	return calls[0]
}

func labelCases() []struct {
	name       string
	mutate     func(e *githubtest.Event)
	wantStart  bool
	wantAuthor github.Author
} {
	owner := github.Author{ID: githubtest.FixtureOwnerID, Type: "User"}
	return []struct {
		name       string
		mutate     func(e *githubtest.Event)
		wantStart  bool
		wantAuthor github.Author
	}{
		{"owner labels own issue", func(*githubtest.Event) {}, true, owner},
		{"label name differs only in case", func(e *githubtest.Event) { e.Label = "Agent" }, true, owner},
		{"stranger labels", func(e *githubtest.Event) { e.SenderID = 666 }, false, github.Author{}},
		{"bot labels", func(e *githubtest.Event) { e.SenderID, e.SenderType = 5555, "Bot" }, false, github.Author{}},
		{"owner applies another label", func(e *githubtest.Event) { e.Label = "bug" }, false, github.Author{}},
		{"closed issue", func(e *githubtest.Event) { e.IssueState = "closed" }, false, github.Author{}},
		{"label on a pull request", func(e *githubtest.Event) { e.IssueIsPR = true }, false, github.Author{}},
		{"owner labels a stranger's issue", func(e *githubtest.Event) {
			e.IssueUserID, e.IssueUserLogin = 666, "someone"
		}, true, github.Author{ID: 666, Type: "User"}},
		{"owner labels an issue an App opened", func(e *githubtest.Event) {
			e.IssueUserID, e.IssueUserType, e.IssueAppID = 5555, "Bot", 88
		}, true, github.Author{ID: 5555, Type: "Bot", AppID: 88}},
	}
}

func TestLabelTrigger(t *testing.T) {
	for _, tt := range labelCases() {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			e := githubtest.LabeledEvent()
			tt.mutate(&e)

			wantStatus(t, h.send(t, "issues", e.JSON()), http.StatusAccepted)

			if !tt.wantStart {
				wantNoCalls(t, h)
				return
			}
			start := onlyCall(t, h).Start
			if start == nil {
				t.Fatal("got a signal, want a start")
			}
			want := github.FeatureStart{
				WorkflowID: fixtureWorkflowID,
				DeliveryID: githubtest.FixtureDeliveryID,
				Repo:       github.RepoRef{ID: githubtest.FixtureRepoID, FullName: githubtest.FixtureRepoFullName, InstallationID: githubtest.FixtureInstallationID},
				Issue:      github.IssueSnapshot{Number: 7, Title: e.IssueTitle, Body: e.IssueBody, Author: tt.wantAuthor},
				LabelerID:  githubtest.FixtureOwnerID,
			}
			if *start != want {
				t.Fatalf("start\n%+v\nwant\n%+v", *start, want)
			}
		})
	}
}

func TestLabelByNonOwnerIgnored(t *testing.T) {
	h := newHarness(t, options{})
	e := githubtest.LabeledEvent()
	e.SenderID = 666

	wantStatus(t, h.send(t, "issues", e.JSON()), http.StatusAccepted)

	wantNoCalls(t, h)
	recs := h.records(t, "reason", "non_owner")
	if len(recs) != 1 || recs[0]["sender_id"] != float64(666) || recs[0]["level"] != "INFO" {
		t.Fatalf("want one INFO non_owner line for sender 666:\n%s", h.logs.String())
	}
}

func TestDuplicateDeliveryMapsToSameWorkflowID(t *testing.T) {
	renamed := "austmsk/tally"
	h := newHarness(t, options{enabled: map[string]bool{githubtest.FixtureRepoFullName: true, renamed: true}})
	event := githubtest.LabeledEvent()
	afterRename := event
	afterRename.RepoFullName = renamed

	h.sendAs(t, "aaaa-0001", "issues", event.JSON())
	h.sendAs(t, "aaaa-0001", "issues", event.JSON())       // the same delivery redelivered
	h.sendAs(t, "aaaa-0002", "issues", event.JSON())       // a new delivery GUID
	h.sendAs(t, "aaaa-0003", "issues", afterRename.JSON()) // after a rename
	h.settle(t)

	calls := h.starter.Calls()
	if len(calls) != 4 {
		t.Fatalf("starter got %d calls, want 4", len(calls))
	}
	for i, c := range calls {
		if c.Start == nil || c.Start.WorkflowID != fixtureWorkflowID {
			t.Fatalf("call %d: %+v, want a start of %s", i, c, fixtureWorkflowID)
		}
		if wantDup := i > 0; errors.Is(c.Err, github.ErrAlreadyStarted) != wantDup {
			t.Fatalf("call %d returned %v, want duplicate %v", i, c.Err, wantDup)
		}
	}
	if n := len(h.records(t, "outcome", "duplicate")); n != 3 {
		t.Errorf("got %d duplicate log lines, want 3:\n%s", n, h.logs.String())
	}
	if n := len(h.records(t, "level", "ERROR")); n != 0 {
		t.Errorf("a duplicate was logged as an error:\n%s", h.logs.String())
	}
}

func TestCommandSignalsFeatureWorkflowID(t *testing.T) {
	h := newHarness(t, options{})
	h.starter.Running(fixtureWorkflowID)
	e := githubtest.CommentEvent("/approve parallel --accept-risk")

	wantStatus(t, h.send(t, "issue_comment", e.JSON()), http.StatusAccepted)

	call := onlyCall(t, h)
	if call.Signal == nil || call.Err != nil {
		t.Fatalf("call %+v, want a delivered signal", call)
	}
	want := github.CommandSignal{
		WorkflowID:  fixtureWorkflowID,
		DeliveryID:  githubtest.FixtureDeliveryID,
		Repo:        github.RepoRef{ID: githubtest.FixtureRepoID, FullName: githubtest.FixtureRepoFullName, InstallationID: githubtest.FixtureInstallationID},
		IssueNumber: 7,
		CommentID:   e.CommentID,
		ActorID:     githubtest.FixtureOwnerID,
		Command:     github.Command{Name: github.CommandApprove, Mode: github.ApproveParallel, AcceptRisk: true},
	}
	if *call.Signal != want {
		t.Fatalf("signal\n%+v\nwant\n%+v", *call.Signal, want)
	}
}

// ignoredComment sends an owner's /approve changed by mutate and expects
// it to be acknowledged, logged with reason, and not dispatched.
func ignoredComment(t *testing.T, mutate func(e *githubtest.Event), reason string) {
	t.Helper()
	h := newHarness(t, options{})
	e := githubtest.CommentEvent("/approve")
	mutate(&e)
	wantStatus(t, h.send(t, "issue_comment", e.JSON()), http.StatusAccepted)
	wantNoCalls(t, h)
	if len(h.records(t, "reason", reason)) != 1 {
		t.Errorf("want one log line with reason %q:\n%s", reason, h.logs.String())
	}
}

func TestEditedCommentIgnored(t *testing.T) {
	for _, action := range []string{"edited", "deleted"} {
		t.Run(action, func(t *testing.T) {
			ignoredComment(t, func(e *githubtest.Event) { e.Action = action }, "unhandled_action")
		})
	}
}

func TestCommandOnPullRequestIgnored(t *testing.T) {
	ignoredComment(t, func(e *githubtest.Event) { e.IssueIsPR = true }, "pull_request")
}

func TestUnknownRepoIgnored(t *testing.T) {
	tests := []struct {
		name     string
		fullName string
		wantCall bool
	}{
		{"another repository", "austmsk/other", false},
		{"no repository", "", false},
		{"enabled repository in another case", "AustMSK/Election-Tally", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			e := githubtest.LabeledEvent()
			e.RepoFullName = tt.fullName
			wantStatus(t, h.send(t, "issues", e.JSON()), http.StatusAccepted)
			if !tt.wantCall {
				wantNoCalls(t, h)
				return
			}
			onlyCall(t, h)
		})
	}
}

func TestWrongInstallationIgnored(t *testing.T) {
	h := newHarness(t, options{})
	e := githubtest.LabeledEvent()
	e.InstallationID = githubtest.FixtureInstallationID + 1

	wantStatus(t, h.send(t, "issues", e.JSON()), http.StatusAccepted)

	wantNoCalls(t, h)
	if len(h.records(t, "reason", "wrong_installation")) != 1 {
		t.Errorf("want one wrong_installation log line:\n%s", h.logs.String())
	}
}

func TestMalformedCommandNotDispatched(t *testing.T) {
	for _, body := range []string{"/approve sideways", "/status now", "/revise"} {
		t.Run(body, func(t *testing.T) {
			ignoredComment(t, func(e *githubtest.Event) { e.CommentBody = body }, "malformed_command")
		})
	}
}

func TestPlainCommentIgnored(t *testing.T) {
	ignoredComment(t, func(e *githubtest.Event) { e.CommentBody = "Looks good, `/approve` when ready." }, "not_command")
}
