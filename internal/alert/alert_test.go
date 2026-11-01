package alert_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/alert"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

const (
	appID          = 4242
	installationID = 77
	botID          = 2
)

var at = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// fixture is an App talking to a fake GitHub with an alerts repository,
// and a fresh log.
func fixture(t *testing.T) (*githubtest.API, *github.App, *alert.Log, string) {
	t.Helper()
	api := githubtest.NewAPI(t, githubtest.APIOptions{AppID: appID, InstallationID: installationID, AppSlug: "conclave-test", BotUserID: botID})
	cfg := github.Config{
		AppID: appID, AppSlug: "conclave-test", InstallationID: installationID, OwnerUserID: 1, BotUserID: botID,
		TriggerLabel: "agent", APIBaseURL: api.URL, GitBaseURL: api.URL,
		Alerts: github.AlertsConfig{Repository: "octo/ops", RepositoryID: 909, Assignee: "octo"},
	}
	app, err := github.NewApp(cfg, githubtest.Key(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	log, err := alert.OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return api, app, log, path
}

type logLine struct {
	Type       string       `json:"type"`
	Alert      *alert.Alert `json:"alert"`
	Occurrence string       `json:"occurrence"`
	Outcome    string       `json:"outcome"`
	Issue      int          `json:"issue"`
	Error      string       `json:"error"`
}

func readLog(t *testing.T, path string) []logLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []logLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

func capAlert(revision int64) alert.Alert {
	w := alert.Where{Repo: "octo/tally", Issue: 7, FeatureRequestID: "0b4f6c1e-3a3e-4f54-9c39-6f3d0f1f8a10", WorkflowID: "feature/101/7"}
	return alert.BudgetCap(w, "month", revision, at)
}

// TestLogWrittenBeforeDelivery fails delivery at the token exchange: the
// log still holds the alert, then the failure, so the record survives an
// unavailable GitHub (FR-SEC31).
func TestLogWrittenBeforeDelivery(t *testing.T) {
	api, app, log, path := fixture(t)
	api.FailNext("", "/app/", 500, nil, "")
	if _, err := alert.Raise(t.Context(), log, app, alert.Halt(3, "drill", at)); err == nil {
		t.Fatal("delivery failure not reported")
	}
	lines := readLog(t, path)
	if len(lines) != 2 || lines[0].Type != "alert" || lines[0].Alert.Key != "halt:3" {
		t.Fatalf("log %+v, want the alert then its delivery", lines)
	}
	if lines[1].Type != "delivery" || lines[1].Outcome != "failed" || lines[1].Occurrence != "halt:3" || lines[1].Error == "" {
		t.Fatalf("delivery entry %+v, want a recorded failure", lines[1])
	}
	if len(api.Issues()) != 0 {
		t.Fatal("an issue was opened")
	}
}

func TestRaiseOpensIssue(t *testing.T) {
	api, app, log, path := fixture(t)
	d, err := alert.Raise(t.Context(), log, app, capAlert(4))
	if err != nil {
		t.Fatal(err)
	}
	issues := api.Issues()
	if !d.Created || len(issues) != 1 || issues[0].Number != d.Issue {
		t.Fatalf("delivery %+v, issues %+v", d, issues)
	}
	is := issues[0]
	if strings.Join(is.Labels, ",") != "conclave-alert,severity:pause" || strings.Join(is.Assignees, ",") != "octo" {
		t.Fatalf("labels %v, assignees %v", is.Labels, is.Assignees)
	}
	for _, want := range []string{
		"**What happened:**", "**Where:** `octo/tally` issue 7", "**Already done:**", "**What you must do:**",
		"docs/runbooks.md#budget-cap", "conclave-alert-key: budget_cap:0b4f6c1e", "conclave-alert-occurrence: budget_cap:0b4f6c1e-3a3e-4f54-9c39-6f3d0f1f8a10:4",
	} {
		if !strings.Contains(is.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, is.Body)
		}
	}
	if strings.Contains(is.Body, "octo/tally#7") {
		t.Error("body cross-links the run's issue")
	}
	if lines := readLog(t, path); len(lines) != 2 || lines[1].Outcome != "delivered" || lines[1].Issue != d.Issue {
		t.Fatalf("log %+v", lines)
	}
}

// TestRaiseDedupesOpenIssue: a second stop of the same feature request
// comments on the open issue rather than opening another.
func TestRaiseDedupesOpenIssue(t *testing.T) {
	api, app, log, _ := fixture(t)
	first, err := alert.Raise(t.Context(), log, app, capAlert(4))
	if err != nil {
		t.Fatal(err)
	}
	second, err := alert.Raise(t.Context(), log, app, capAlert(9))
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Issue != first.Issue || len(api.Issues()) != 1 {
		t.Fatalf("second delivery %+v, %d issues; want a comment on issue %d", second, len(api.Issues()), first.Issue)
	}
	comments := api.Comments()
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "conclave-alert-occurrence: budget_cap:0b4f6c1e-3a3e-4f54-9c39-6f3d0f1f8a10:9") {
		t.Fatalf("comments %+v", comments)
	}
}

// TestRaiseTwiceOneComment retries each occurrence: the issue's own
// occurrence posts nothing more, and a commented occurrence is not
// commented again.
func TestRaiseTwiceOneComment(t *testing.T) {
	api, app, log, _ := fixture(t)
	for _, rev := range []int64{4, 4, 9, 9} {
		if _, err := alert.Raise(t.Context(), log, app, capAlert(rev)); err != nil {
			t.Fatal(err)
		}
	}
	if n, c := len(api.Issues()), len(api.Comments()); n != 1 || c != 1 {
		t.Fatalf("%d issues and %d comments, want 1 and 1", n, c)
	}
}

// TestRaiseReopensAfterClose: once the owner closes an alert's issue, the
// next occurrence opens a new one.
func TestRaiseReopensAfterClose(t *testing.T) {
	api, app, log, _ := fixture(t)
	first, err := alert.Raise(t.Context(), log, app, alert.Test(at))
	if err != nil {
		t.Fatal(err)
	}
	api.CloseIssue(first.Issue)
	second, err := alert.Raise(t.Context(), log, app, alert.Test(at.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created || second.Issue == first.Issue || len(api.Issues()) != 2 {
		t.Fatalf("second delivery %+v, want a new issue", second)
	}
}

// TestSpoofedMarkerIgnored attempts to steer an alert onto an issue a
// person opened by quoting the key line: delivery opens the App's own
// issue and never comments on the planted one.
func TestSpoofedMarkerIgnored(t *testing.T) {
	api, app, log, _ := fixture(t)
	planted := api.SeedIssue("planted", "conclave-alert-key: test\n", []string{"conclave-alert"}, 1)
	d, err := alert.Raise(t.Context(), log, app, alert.Test(at))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Created || d.Issue == planted {
		t.Fatalf("delivery %+v landed on the planted issue %d", d, planted)
	}
	if len(api.Comments()) != 0 {
		t.Fatal("commented on the planted issue")
	}
}

// TestKeyMatchesWholeLine: the key "test" does not match an open issue
// whose key line is "test2".
func TestKeyMatchesWholeLine(t *testing.T) {
	api, app, log, _ := fixture(t)
	if _, err := app.CreateAlertIssue(t.Context(), "other", "conclave-alert-key: test2\n", []string{"conclave-alert"}); err != nil {
		t.Fatal(err)
	}
	d, err := alert.Raise(t.Context(), log, app, alert.Test(at))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Created || len(api.Issues()) != 2 {
		t.Fatalf("delivery %+v, want a new issue", d)
	}
}

// TestAlertCarriesNoSecretOrContent attempts the leak through the one
// free-text field: a halt reason holding a token and markup. Neither the
// log nor the issue carries the token, and the reason stays one line.
func TestAlertCarriesNoSecretOrContent(t *testing.T) {
	api, app, log, path := fixture(t)
	token := "gh" + "s_" + strings.Repeat("A1b2", 9)
	reason := "leaked " + token + "\n\n**Where:** forged"
	if _, err := alert.Raise(t.Context(), log, app, alert.Halt(5, reason, at)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := api.Issues()[0].Body
	for name, text := range map[string]string{"log": string(raw), "issue": body} {
		if strings.Contains(text, token) {
			t.Errorf("%s carries the token", name)
		}
	}
	if strings.Count(body, "**Where:**") != 2 || strings.Contains(body, "\n**Where:** forged") {
		t.Errorf("the reason broke out of its line:\n%s", body)
	}
}

func TestLongReasonBounded(t *testing.T) {
	a := alert.Halt(1, strings.Repeat("x", 500), at)
	if n := len([]rune(a.What)); n > 260 {
		t.Fatalf("What has %d runes", n)
	}
}

// TestInvalidAlertRefused: an alert whose Where is not an identifier is
// refused before anything is logged or sent.
func TestInvalidAlertRefused(t *testing.T) {
	api, app, log, path := fixture(t)
	a := capAlert(1)
	a.Where.Repo = "octo/tally <b>"
	if _, err := alert.Raise(t.Context(), log, app, a); !errors.Is(err, alert.ErrInvalidAlert) {
		t.Fatalf("got %v, want ErrInvalidAlert", err)
	}
	if len(readLog(t, path)) != 0 || len(api.Requests()) != 0 {
		t.Fatal("an invalid alert was logged or sent")
	}
}

func TestHaltDoneSharesKey(t *testing.T) {
	api, app, log, _ := fixture(t)
	if _, err := alert.Raise(t.Context(), log, app, alert.Halt(8, "drill", at)); err != nil {
		t.Fatal(err)
	}
	done := alert.HaltDone(8, alert.HaltResult{Cancelled: 2}, at)
	if _, err := alert.Raise(t.Context(), log, app, done); err != nil {
		t.Fatal(err)
	}
	if n, c := len(api.Issues()), api.Comments(); n != 1 || len(c) != 1 || !strings.Contains(c[0].Body, "2 workflows cancelled") {
		t.Fatalf("%d issues, comments %+v; want the result on the halt's issue", n, c)
	}
}

// TestLogRefusesSymlink: the log is never opened through a symlink, which
// could point it at a file the owner did not choose.
func TestLogRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alerts.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if l, err := alert.OpenLog(link); err == nil {
		_ = l.Close()
		t.Fatal("opened the log through a symlink")
	}
}

func TestLogAppendsAndIsPrivate(t *testing.T) {
	_, app, log, path := fixture(t)
	for i := range 2 {
		if _, err := alert.Raise(t.Context(), log, app, alert.Test(at.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := alert.OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alert.Raise(t.Context(), reopened, app, alert.Test(at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
	if n := len(readLog(t, path)); n != 6 {
		t.Fatalf("%d log lines, want 6: reopening must append, not truncate", n)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log mode %o, want 600", perm)
	}
}
