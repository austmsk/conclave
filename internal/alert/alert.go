package alert

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/austmsk/conclave/internal/github"
)

// Severity is how much an alert stops (FR-SEC31).
type Severity string

const (
	// SeverityHalt stops all work: the kill switch.
	SeverityHalt Severity = "halt"
	// SeverityPause stops one feature request: a budget cap.
	SeverityPause Severity = "pause"
	// SeverityDigest stops nothing. Milestone 1 delivers it at once; the
	// weekly batch is deferred (decision 0016).
	SeverityDigest Severity = "digest"
)

// Label is on every alert issue; LabelSeverity adds the severity's own.
const Label = "conclave-alert"

// LabelSeverity is the label naming s, such as "severity:halt".
func LabelSeverity(s Severity) string { return "severity:" + string(s) }

// RunbookBase is where alert runbooks live. Runbooks are platform
// documents in the public repository and hold nothing about any run.
const RunbookBase = "https://github.com/austmsk/conclave/blob/main/docs/runbooks.md"

// maxReasonRunes bounds the owner's halt reason.
const maxReasonRunes = 200

// ErrInvalidAlert means an alert is missing a field or carries a value no
// constructor would produce.
var ErrInvalidAlert = errors.New("invalid alert")

// identifier is the shape of a key or an occurrence ID: what a constructor
// builds from IDs, never free text.
var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9:/._-]{0,199}$`)

// whereValue is the shape of a Where string: a repository full name, a
// UUID, or a workflow ID. Anything with spaces or markup is not an
// identifier and is refused.
var whereValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:/._-]{0,199}$`)

// Where names what an alert is about, by identifier only. Unset fields
// are left out of the alert's text.
type Where struct {
	Repo             string `json:"repo,omitempty"`
	Issue            int    `json:"issue,omitempty"`
	FeatureRequestID string `json:"feature_request_id,omitempty"`
	WorkflowID       string `json:"workflow_id,omitempty"`
	HaltID           int64  `json:"halt_id,omitempty"`
}

// Alert is one occurrence of an alert. Key names the condition, so a
// repeat updates the same open issue; OccurrenceID names this occurrence,
// so a retried delivery does not post it twice.
type Alert struct {
	OccurrenceID string    `json:"occurrence"`
	Key          string    `json:"key"`
	Severity     Severity  `json:"severity"`
	Title        string    `json:"title"`
	What         string    `json:"what"`
	Where        Where     `json:"where"`
	Action       string    `json:"action"`
	OwnerAction  string    `json:"owner_action"`
	Runbook      string    `json:"runbook"`
	At           time.Time `json:"at"`
}

// Halt is the alert `conclave halt` raises before it cancels anything.
// reason is the owner's own text: redacted, made one line, and bounded.
func Halt(haltID int64, reason string, at time.Time) Alert {
	id := "halt:" + strconv.FormatInt(haltID, 10)
	return Alert{
		OccurrenceID: id, Key: id, Severity: SeverityHalt,
		Title:       "Kill switch: all work halted",
		What:        "The owner ran conclave halt: " + ownerText(reason),
		Where:       Where{HaltID: haltID},
		Action:      "Halt recorded. Every open workflow is being cancelled, workers stop after their grace period, and every sandbox is swept.",
		OwnerAction: "Follow the incident runbook if this was not a drill. Run conclave resume when it is safe to start work again.",
		Runbook:     RunbookBase + "#halt",
		At:          at,
	}
}

// HaltResult is what a halt did, for the follow-up on its alert issue.
type HaltResult struct {
	Cancelled  int
	StillOpen  int
	Released   int
	Reaped     int
	Unreleased int
	Containers int
}

// HaltDone is the follow-up to Halt with what the halt did. It shares
// Halt's key, so it lands on the same issue as a comment.
func HaltDone(haltID int64, r HaltResult, at time.Time) Alert {
	a := Halt(haltID, "-", at)
	a.OccurrenceID += ":done"
	a.Title = "Kill switch: halt complete"
	a.What = "The halt finished its sweep."
	a.Action = fmt.Sprintf("%d workflows cancelled, %d still open; %d recorded sandboxes released, %d unrecorded containers reaped; afterwards %d attempts unreleased and %d labelled containers remain.",
		r.Cancelled, r.StillOpen, r.Released, r.Reaped, r.Unreleased, r.Containers)
	a.OwnerAction = "If anything is still open or remains, see the runbook before resuming."
	if r.StillOpen == 0 && r.Unreleased == 0 && r.Containers == 0 {
		a.OwnerAction = "Nothing remains. Run conclave resume when it is safe to start work again."
	}
	return a
}

// BudgetCap is the pause alert for a feature request stopped by a budget
// cap. revision is the feature request's revision after the move, so each
// stop is its own occurrence and a /retry that stops again comments on the
// same open issue.
func BudgetCap(w Where, capKind string, revision int64, at time.Time) Alert {
	key := "budget_cap:" + w.FeatureRequestID
	return Alert{
		OccurrenceID: key + ":" + strconv.FormatInt(revision, 10), Key: key, Severity: SeverityPause,
		Title:       "Budget cap: a feature request is paused",
		What:        "A model call was refused because the " + capName(capKind) + " cap would be passed.",
		Where:       w,
		Action:      "The feature request moved to needs_attention. Its attempts and recorded spend are kept.",
		OwnerAction: "If the spend is expected, raise the cap in router.yaml and restart the worker, then comment /retry on the issue. Otherwise comment /cancel.",
		Runbook:     RunbookBase + "#budget-cap",
		At:          at,
	}
}

// Test is the alert `conclave alert-test` raises to prove the path. Its
// key is fixed, so a second test comments on the first's issue while it
// is open.
func Test(at time.Time) Alert {
	return Alert{
		OccurrenceID: "test:" + strconv.FormatInt(at.UnixNano(), 10), Key: "test", Severity: SeverityDigest,
		Title:       "Alert test",
		What:        "The owner ran conclave alert-test.",
		Action:      "None. This alert proves the local log and issue delivery work.",
		OwnerAction: "Close this issue.",
		Runbook:     RunbookBase + "#alert-test",
		At:          at,
	}
}

func capName(kind string) string {
	switch kind {
	case "attempt", "feature_request", "month":
		return strings.ReplaceAll(kind, "_", " ")
	}
	return "budget"
}

// ownerText makes the owner's free text safe for the log and an issue:
// token-shaped strings redacted, one line, bounded.
func ownerText(s string) string {
	s = strings.Join(strings.Fields(github.RedactText(s)), " ")
	if utf8.RuneCountInString(s) > maxReasonRunes {
		s = string([]rune(s)[:maxReasonRunes]) + "…"
	}
	if s == "" {
		return "(no reason given)"
	}
	return s
}

func (a Alert) validate() error {
	switch {
	case !identifier.MatchString(a.OccurrenceID) || !identifier.MatchString(a.Key):
		return fmt.Errorf("%w: key %q, occurrence %q", ErrInvalidAlert, a.Key, a.OccurrenceID)
	case a.Severity != SeverityHalt && a.Severity != SeverityPause && a.Severity != SeverityDigest:
		return fmt.Errorf("%w: severity %q", ErrInvalidAlert, a.Severity)
	case a.Title == "" || a.What == "" || a.Action == "" || a.OwnerAction == "" || a.Runbook == "":
		return fmt.Errorf("%w: %s is missing text", ErrInvalidAlert, a.OccurrenceID)
	case a.At.IsZero():
		return fmt.Errorf("%w: %s has no time", ErrInvalidAlert, a.OccurrenceID)
	}
	for _, v := range []string{a.Where.Repo, a.Where.FeatureRequestID, a.Where.WorkflowID} {
		if v != "" && !whereValue.MatchString(v) {
			return fmt.Errorf("%w: %s names %q, which is not an identifier", ErrInvalidAlert, a.OccurrenceID, v)
		}
	}
	return nil
}
