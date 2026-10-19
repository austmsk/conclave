package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Reasons a verified delivery is acknowledged and ignored. They appear in
// the log as reason, so an owner can see why a label or a command did
// nothing without the log ever carrying the text that was ignored.
const (
	reasonPing             = "ping"
	reasonUnknownRepo      = "unknown_repo"
	reasonWrongInstall     = "wrong_installation"
	reasonUnhandledEvent   = "unhandled_event"
	reasonUnhandledAction  = "unhandled_action"
	reasonOtherLabel       = "other_label"
	reasonPullRequest      = "pull_request"
	reasonIssueClosed      = "issue_closed"
	reasonIncomplete       = "incomplete_payload"
	reasonNonOwner         = "non_owner"
	reasonBot              = "bot"
	reasonPlatform         = "platform"
	reasonSenderMismatch   = "sender_mismatch"
	reasonNotCommand       = "not_command"
	reasonMalformedCommand = "malformed_command"
)

// The payload types below decode only the fields a decision rests on.
// Logins are deliberately absent: they change, and nothing here may decide
// on one (FR-SEC1).

type eventPayload struct {
	Action       string       `json:"action"`
	Repository   eventRepo    `json:"repository"`
	Installation eventInstall `json:"installation"`
	Sender       eventUser    `json:"sender"`
	Issue        eventIssue   `json:"issue"`
	Label        eventLabel   `json:"label"`
	Comment      eventComment `json:"comment"`
}

type eventRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

type eventInstall struct {
	ID int64 `json:"id"`
}

type eventUser struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// eventApp is performed_via_github_app. A nil pointer means the action was
// not performed through an App.
type eventApp struct {
	ID int64 `json:"id"`
}

func (a *eventApp) id() int64 {
	if a == nil {
		return 0
	}
	return a.ID
}

type eventIssue struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	Body   string    `json:"body"`
	State  string    `json:"state"`
	User   eventUser `json:"user"`
	Via    *eventApp `json:"performed_via_github_app"`
	// PullRequest is present only when the issue is a pull request; its
	// contents are never read.
	PullRequest *struct{} `json:"pull_request"`
}

type eventLabel struct {
	Name string `json:"name"`
}

type eventComment struct {
	ID   int64     `json:"id"`
	Body string    `json:"body"`
	User eventUser `json:"user"`
	Via  *eventApp `json:"performed_via_github_app"`
}

// ignore is a verified delivery that asks for nothing: why, and which
// account sent it, for the log.
type ignore struct {
	reason   string
	senderID int64
}

// classify decides what a verified delivery asks for: a run to start, a
// command to deliver, or nothing. It is pure, so every rule is testable
// from a payload alone. Identity comes from the payload's numeric IDs,
// account types and App fields only; neither the issue nor the comment
// body is ever read for who wrote it, and neither is returned in an
// ignore, so neither reaches a log.
//
// classify returns an error only for a body that is not a JSON object of
// the expected shape.
func classify(cfg Config, enabled map[string]bool, event, deliveryID string, body []byte) (job, ignore, error) {
	if event == "ping" {
		return job{}, ignore{reason: reasonPing}, nil
	}
	var p eventPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return job{}, ignore{}, fmt.Errorf("decoding %s delivery: %w", eventName(event), err)
	}
	skip := ignore{senderID: p.Sender.ID}
	if !enabled[strings.ToLower(p.Repository.FullName)] {
		skip.reason = reasonUnknownRepo
		return job{}, skip, nil
	}
	if p.Installation.ID != cfg.InstallationID {
		skip.reason = reasonWrongInstall
		return job{}, skip, nil
	}
	repo := RepoRef{ID: p.Repository.ID, FullName: p.Repository.FullName, InstallationID: p.Installation.ID}
	var reason string
	var j job
	switch event {
	case "issues":
		j, reason = classifyLabel(cfg, &p, repo, deliveryID)
	case "issue_comment":
		j, reason = classifyComment(cfg, &p, repo, deliveryID)
	default:
		reason = reasonUnhandledEvent
	}
	skip.reason = reason
	return j, skip, nil
}

// classifyLabel starts a run when the owner applies the trigger label to an
// open issue (FR-S1). The issue's own author may be anyone; the snapshot
// records who, so the body is later quarantined as that author's text.
func classifyLabel(cfg Config, p *eventPayload, repo RepoRef, deliveryID string) (job, string) {
	switch {
	case p.Action != "labeled":
		return job{}, reasonUnhandledAction
	case !strings.EqualFold(p.Label.Name, cfg.TriggerLabel):
		return job{}, reasonOtherLabel
	case p.Issue.PullRequest != nil:
		return job{}, reasonPullRequest
	}
	sender := Author{ID: p.Sender.ID, Type: p.Sender.Type}
	if src := Classify(sender, cfg.OwnerUserID, cfg.AppID); src != SourceOwner {
		return job{}, sourceReason(src)
	}
	if p.Issue.State != "open" {
		return job{}, reasonIssueClosed
	}
	if repo.ID <= 0 || p.Issue.Number <= 0 {
		return job{}, reasonIncomplete
	}
	start := &FeatureStart{
		WorkflowID: FeatureWorkflowID(repo.ID, p.Issue.Number),
		DeliveryID: deliveryID,
		Repo:       repo,
		Issue: IssueSnapshot{
			Number: p.Issue.Number,
			Title:  p.Issue.Title,
			Body:   p.Issue.Body,
			Author: Author{ID: p.Issue.User.ID, Type: p.Issue.User.Type, AppID: p.Issue.Via.id()},
		},
		LabelerID: p.Sender.ID,
	}
	return job{start: start}, ""
}

// classifyComment turns an owner's new comment on an issue into a command
// signal. The comment's author must be the owner acting directly, not
// through any App, and the event's sender must be that same account, so a
// payload whose comment and sender disagree is refused rather than
// guessed at (FR-SEC1, FR-SEC28).
func classifyComment(cfg Config, p *eventPayload, repo RepoRef, deliveryID string) (job, string) {
	if p.Action != "created" {
		return job{}, reasonUnhandledAction
	}
	if p.Issue.PullRequest != nil {
		return job{}, reasonPullRequest
	}
	author := Author{ID: p.Comment.User.ID, Type: p.Comment.User.Type, AppID: p.Comment.Via.id()}
	if src := Classify(author, cfg.OwnerUserID, cfg.AppID); src != SourceOwner {
		return job{}, sourceReason(src)
	}
	if p.Sender.ID != p.Comment.User.ID || p.Sender.Type != "User" {
		return job{}, reasonSenderMismatch
	}
	cmd, ok, err := ParseCommand(p.Comment.Body)
	if !ok {
		return job{}, reasonNotCommand
	}
	if err != nil {
		return job{}, reasonMalformedCommand
	}
	if repo.ID <= 0 || p.Issue.Number <= 0 || p.Comment.ID <= 0 {
		return job{}, reasonIncomplete
	}
	signal := &CommandSignal{
		WorkflowID:  FeatureWorkflowID(repo.ID, p.Issue.Number),
		DeliveryID:  deliveryID,
		Repo:        repo,
		IssueNumber: p.Issue.Number,
		CommentID:   p.Comment.ID,
		ActorID:     p.Sender.ID,
		Command:     cmd,
	}
	return job{signal: signal}, ""
}

// sourceReason names a non-owner Source for the log.
func sourceReason(src Source) string {
	switch src {
	case SourcePlatform:
		return reasonPlatform
	case SourceBot:
		return reasonBot
	default:
		return reasonNonOwner
	}
}

// knownEvents are the event names this package logs verbatim. Any other
// header value is logged as "other": the header is the sender's text.
var knownEvents = map[string]bool{"ping": true, "issues": true, "issue_comment": true}

func eventName(event string) string {
	if knownEvents[event] {
		return event
	}
	return "other"
}
