package githubtest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

// The fixture installation: one owner, one App, one enabled repository.
const (
	FixtureRepoID         int64 = 812345678
	FixtureRepoFullName         = "austmsk/election-tally"
	FixtureInstallationID int64 = 4242
	FixtureOwnerID        int64 = 1001
	FixtureOwnerLogin           = "austmsk"
	FixtureAppID          int64 = 77
	FixtureAppSlug              = "conclave"
	// FixtureAppBotID is the user ID of the App's bot account, which
	// authors the App's comments.
	FixtureAppBotID int64 = 9001
	// FixtureDeliveryID is the X-GitHub-Delivery header Request sets.
	FixtureDeliveryID = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
)

// WebhookConfig returns a valid configuration for the fixture installation.
func WebhookConfig() github.Config {
	return github.Config{
		AppID:          FixtureAppID,
		AppSlug:        FixtureAppSlug,
		InstallationID: FixtureInstallationID,
		OwnerUserID:    FixtureOwnerID,
		TriggerLabel:   github.DefaultTriggerLabel,
		APIBaseURL:     github.DefaultAPIBaseURL,
		GitBaseURL:     github.DefaultGitBaseURL,
	}
}

// EnabledRepos returns the fixture repository as the only enabled one.
func EnabledRepos() map[string]bool {
	return map[string]bool{FixtureRepoFullName: true}
}

// Sign returns the X-Hub-Signature-256 value GitHub would send for body.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Request returns a delivery of event carrying body, signed with secret,
// with the headers GitHub sets. Tests change a header to forge one.
func Request(t testing.TB, secret, event string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", FixtureDeliveryID)
	req.Header.Set("X-Hub-Signature-256", Sign([]byte(secret), body))
	return req
}

// Event is an issues or issue_comment delivery with every field the
// receiver decides on settable. LabeledEvent and CommentEvent fill in the
// fixture's defaults; a test changes only the field its case is about.
type Event struct {
	Action         string
	RepoID         int64
	RepoFullName   string
	InstallationID int64

	SenderID    int64
	SenderType  string
	SenderLogin string

	IssueNumber    int
	IssueTitle     string
	IssueBody      string
	IssueState     string
	IssueUserID    int64
	IssueUserType  string
	IssueUserLogin string
	// IssueAppID is the issue's performed_via_github_app.id; 0 is null.
	IssueAppID int64
	// IssueIsPR adds a pull_request object to the issue.
	IssueIsPR bool

	// Label is the applied label; empty omits the label object.
	Label string

	// CommentID 0 omits the comment object.
	CommentID        int64
	CommentBody      string
	CommentUserID    int64
	CommentUserType  string
	CommentUserLogin string
	// CommentAppID is the comment's performed_via_github_app.id; 0 is null.
	CommentAppID int64
}

// LabeledEvent is the owner applying the trigger label to their own open
// issue 7.
func LabeledEvent() Event {
	return Event{
		Action:         "labeled",
		RepoID:         FixtureRepoID,
		RepoFullName:   FixtureRepoFullName,
		InstallationID: FixtureInstallationID,
		SenderID:       FixtureOwnerID,
		SenderType:     "User",
		SenderLogin:    FixtureOwnerLogin,
		IssueNumber:    7,
		IssueTitle:     "Tally rejects ballots with a trailing newline",
		IssueBody:      "Steps to reproduce: upload a ballot file ending in a newline.",
		IssueState:     "open",
		IssueUserID:    FixtureOwnerID,
		IssueUserType:  "User",
		IssueUserLogin: FixtureOwnerLogin,
		Label:          github.DefaultTriggerLabel,
	}
}

// CommentEvent is the owner commenting body on their open issue 7.
func CommentEvent(body string) Event {
	e := LabeledEvent()
	e.Action = "created"
	e.Label = ""
	e.CommentID = 5550001
	e.CommentBody = body
	e.CommentUserID = FixtureOwnerID
	e.CommentUserType = "User"
	e.CommentUserLogin = FixtureOwnerLogin
	return e
}

// JSON encodes the event as GitHub would, with null for an absent
// performed_via_github_app.
func (e Event) JSON() []byte {
	issue := map[string]any{
		"number":                   e.IssueNumber,
		"title":                    e.IssueTitle,
		"body":                     e.IssueBody,
		"state":                    e.IssueState,
		"user":                     user(e.IssueUserID, e.IssueUserType, e.IssueUserLogin),
		"performed_via_github_app": app(e.IssueAppID),
	}
	if e.IssueIsPR {
		issue["pull_request"] = map[string]any{"url": "https://api.github.com/repos/" + e.RepoFullName + "/pulls/1"}
	}
	payload := map[string]any{
		"action":       e.Action,
		"repository":   map[string]any{"id": e.RepoID, "full_name": e.RepoFullName},
		"installation": map[string]any{"id": e.InstallationID},
		"sender":       user(e.SenderID, e.SenderType, e.SenderLogin),
		"issue":        issue,
	}
	if e.Label != "" {
		payload["label"] = map[string]any{"name": e.Label}
	}
	if e.CommentID != 0 {
		payload["comment"] = map[string]any{
			"id":                       e.CommentID,
			"body":                     e.CommentBody,
			"user":                     user(e.CommentUserID, e.CommentUserType, e.CommentUserLogin),
			"performed_via_github_app": app(e.CommentAppID),
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		panic("githubtest: encoding event: " + err.Error())
	}
	return out
}

func user(id int64, typ, login string) map[string]any {
	return map[string]any{"id": id, "type": typ, "login": login}
}

func app(id int64) any {
	if id == 0 {
		return nil
	}
	return map[string]any{"id": id, "slug": "app-" + strconv.FormatInt(id, 10)}
}
