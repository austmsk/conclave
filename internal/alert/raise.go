package alert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/github"
)

// Issues is what delivery needs from GitHub: the alert calls of
// *github.App, which reach only the operations repository.
type Issues interface {
	CreateAlertIssue(ctx context.Context, title, body string, labels []string) (github.Issue, error)
	ListOpenAlertIssues(ctx context.Context, label string) ([]github.Issue, error)
	CommentOnAlertIssue(ctx context.Context, issue int, body string) (github.Comment, error)
	AlertIssueComments(ctx context.Context, issue int) ([]github.Comment, error)
}

// Delivery is where an alert landed.
type Delivery struct {
	Issue int
	URL   string
	// Created is true when a new issue was opened, false when an open one
	// was updated or already held this occurrence.
	Created bool
}

// Raise writes a to the log, then delivers it, then records the outcome.
// A log write that fails stops Raise before delivery: an alert the local
// record does not hold is never sent. A failed delivery is recorded and
// returned; the caller decides whether to retry, and a retry with the same
// alert is safe.
func Raise(ctx context.Context, log *Log, issues Issues, a Alert) (Delivery, error) {
	if err := a.validate(); err != nil {
		return Delivery{}, err
	}
	if err := log.appendAlert(a); err != nil {
		return Delivery{}, fmt.Errorf("raising alert %s: %w", a.OccurrenceID, err)
	}
	d, derr := deliver(ctx, issues, a)
	if lerr := log.appendDelivery(a.OccurrenceID, time.Now().UTC(), d, derr); lerr != nil {
		return d, errors.Join(derr, fmt.Errorf("recording delivery of %s: %w", a.OccurrenceID, lerr))
	}
	if derr != nil {
		return Delivery{}, fmt.Errorf("delivering alert %s: %w", a.OccurrenceID, derr)
	}
	return d, nil
}

func keyLine(key string) string       { return "conclave-alert-key: " + key }
func occurrenceLine(id string) string { return "conclave-alert-occurrence: " + id }

func deliver(ctx context.Context, issues Issues, a Alert) (Delivery, error) {
	open, err := issues.ListOpenAlertIssues(ctx, Label)
	if err != nil {
		return Delivery{}, err
	}
	for _, is := range open {
		if !hasLine(is.Body, keyLine(a.Key)) {
			continue
		}
		return update(ctx, issues, is, a)
	}
	is, err := issues.CreateAlertIssue(ctx, a.Title, body(a), []string{Label, LabelSeverity(a.Severity)})
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{Issue: is.Number, URL: is.URL, Created: true}, nil
}

// update comments a on the open issue for its key, unless the issue or
// one of the App's comments on it already holds this occurrence.
func update(ctx context.Context, issues Issues, is github.Issue, a Alert) (Delivery, error) {
	d := Delivery{Issue: is.Number, URL: is.URL}
	if hasLine(is.Body, occurrenceLine(a.OccurrenceID)) {
		return d, nil
	}
	comments, err := issues.AlertIssueComments(ctx, is.Number)
	if err != nil {
		return Delivery{}, err
	}
	for _, c := range comments {
		if hasLine(c.Body, occurrenceLine(a.OccurrenceID)) {
			return d, nil
		}
	}
	if _, err := issues.CommentOnAlertIssue(ctx, is.Number, "**"+a.Title+"**\n\n"+body(a)); err != nil {
		return Delivery{}, err
	}
	return d, nil
}

// hasLine reports whether text holds line as a whole line, so the key
// "test" does not match "test:2".
func hasLine(text, line string) bool {
	for l := range strings.Lines(text) {
		if strings.TrimRight(l, "\r\n") == line {
			return true
		}
	}
	return false
}

// body renders an alert as FR-SEC31 asks: what happened, where, what was
// already done, what the owner must do, and the runbook, ending with the
// key and occurrence lines delivery matches on.
func body(a Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Severity:** %s\n\n", a.Severity)
	fmt.Fprintf(&b, "**What happened:** %s\n\n", a.What)
	if w := where(a.Where); w != "" {
		fmt.Fprintf(&b, "**Where:** %s\n\n", w)
	}
	fmt.Fprintf(&b, "**Already done:** %s\n\n", a.Action)
	fmt.Fprintf(&b, "**What you must do:** %s\n\n", a.OwnerAction)
	fmt.Fprintf(&b, "**Runbook:** %s\n\n", a.Runbook)
	fmt.Fprintf(&b, "**When:** %s\n\n", a.At.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "%s\n%s\n", keyLine(a.Key), occurrenceLine(a.OccurrenceID))
	return b.String()
}

func where(w Where) string {
	var parts []string
	if w.Repo != "" && w.Issue > 0 {
		// Not owner/name#n: GitHub would cross-link the run's issue to
		// this one.
		parts = append(parts, fmt.Sprintf("`%s` issue %d", w.Repo, w.Issue))
	}
	if w.FeatureRequestID != "" {
		parts = append(parts, "feature request `"+w.FeatureRequestID+"`")
	}
	if w.WorkflowID != "" {
		parts = append(parts, "workflow `"+w.WorkflowID+"`")
	}
	if w.HaltID > 0 {
		parts = append(parts, fmt.Sprintf("halt %d", w.HaltID))
	}
	return strings.Join(parts, ", ")
}

func redactError(err error) string { return github.RedactText(err.Error()) }
