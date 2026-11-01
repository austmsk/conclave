package github_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

const (
	alertsRepoID = 909
	restBotID    = 2
)

func alertsOptions() githubtest.APIOptions {
	o := restOptions()
	o.BotUserID = restBotID
	return o
}

func alertsConfig(base string) github.Config {
	cfg := restConfig(base)
	cfg.Alerts = github.AlertsConfig{Repository: "octo/ops", RepositoryID: alertsRepoID, Assignee: "octo-owner"}
	return cfg
}

// alertsFixture is restFixture with the alerts repository configured.
func alertsFixture(t *testing.T, opts githubtest.APIOptions) (*githubtest.API, *github.App) {
	t.Helper()
	api := githubtest.NewAPI(t, opts)
	app, err := github.NewApp(alertsConfig(api.URL), githubtest.Key(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	return api, app
}

// TestAlertTokenAsksOnlyIssues: a token for the alerts repository asks
// for issues and metadata only, scoped to that repository (RL-18).
func TestAlertTokenAsksOnlyIssues(t *testing.T) {
	api, app := alertsFixture(t, alertsOptions())
	if _, err := app.ListOpenAlertIssues(t.Context(), "conclave-alert"); err != nil {
		t.Fatal(err)
	}
	mints := api.RequestsTo("/app/")
	want := `{"repository_ids":[909],"permissions":{"issues":"write","metadata":"read"}}`
	if len(mints) != 1 || !sameJSON(t, mints[0].Body, []byte(want)) {
		t.Fatalf("token requests %+v, want one with body %s", mints, want)
	}
}

// TestAlertTokenWithContentsRefused: a token for the alerts repository
// that could push is discarded unused, though contents is in the run set.
func TestAlertTokenWithContentsRefused(t *testing.T) {
	opts := alertsOptions()
	opts.ExtraPermissions = map[string]string{"contents": "write"}
	api, app := alertsFixture(t, opts)
	_, err := app.CreateAlertIssue(t.Context(), "Halt", "body", []string{"conclave-alert"})
	if !errors.Is(err, github.ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	if n := countRequests(api, http.MethodPost, "/repos/"); n != 0 {
		t.Fatalf("%d repository writes sent with the wide token", n)
	}
}

// TestRunTokenPermissionsUnchanged: configuring an alerts repository does
// not change what a run repository's token asks for.
func TestRunTokenPermissionsUnchanged(t *testing.T) {
	api, app := alertsFixture(t, alertsOptions())
	if _, err := app.CreateComment(t.Context(), restRepo, 7, "status"); err != nil {
		t.Fatal(err)
	}
	want := `{"repository_ids":[101],"permissions":{"checks":"read","contents":"write","issues":"write","pull_requests":"write","metadata":"read"}}`
	if mints := api.RequestsTo("/app/"); len(mints) != 1 || !sameJSON(t, mints[0].Body, []byte(want)) {
		t.Fatalf("token requests %+v, want %s", mints, want)
	}
}

func TestCreateAlertIssue(t *testing.T) {
	api, app := alertsFixture(t, alertsOptions())
	is, err := app.CreateAlertIssue(t.Context(), "Kill switch", "halted, paging @octo/everyone", []string{"conclave-alert", "severity:halt"})
	if err != nil {
		t.Fatal(err)
	}
	got := api.Issues()
	if len(got) != 1 || got[0].Number != is.Number || got[0].Title != "Kill switch" {
		t.Fatalf("issues %+v, want the one created (%+v)", got, is)
	}
	if !slices.Equal(got[0].Labels, []string{"conclave-alert", "severity:halt"}) || !slices.Equal(got[0].Assignees, []string{"octo-owner"}) {
		t.Fatalf("labels %v assignees %v", got[0].Labels, got[0].Assignees)
	}
	if !strings.Contains(got[0].Body, "`@octo/everyone`") {
		t.Fatalf("body was not sanitized: %q", got[0].Body)
	}
	if reqs := api.RequestsTo("/repos/octo/ops/issues"); len(reqs) != 1 {
		t.Fatalf("requests %+v, want one to the alerts repository", reqs)
	}
}

func TestCreateAlertIssueRefusals(t *testing.T) {
	tests := []struct {
		name, title, body string
		labels            []string
		want              error
	}{
		{"label with a space", "t", "b", []string{"severity halt"}, github.ErrRefused},
		{"upper-case label", "t", "b", []string{"Alert"}, github.ErrRefused},
		{"empty title", "", "b", nil, github.ErrInvalidRequest},
		{"long title", strings.Repeat("x", 257), "b", nil, github.ErrInvalidRequest},
		{"token in body", "t", "leak " + githubtest.CanaryToken, nil, github.ErrRefused},
		{"token in title", "leak " + githubtest.CanaryToken, "b", nil, github.ErrRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api, app := alertsFixture(t, alertsOptions())
			if _, err := app.CreateAlertIssue(t.Context(), tt.title, tt.body, tt.labels); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if len(api.Issues()) != 0 {
				t.Fatal("an issue was created")
			}
		})
	}
}

// TestListOpenAlertIssuesKeepsOwn: only open issues the App's bot opened
// with the label are listed. One a person opened that quotes the same
// text, and one another App opened, are left out.
func TestListOpenAlertIssuesKeepsOwn(t *testing.T) {
	api, app := alertsFixture(t, alertsOptions())
	mine, err := app.CreateAlertIssue(t.Context(), "Mine", "conclave-alert-key: k", []string{"conclave-alert"})
	if err != nil {
		t.Fatal(err)
	}
	api.SeedIssue("Person", "conclave-alert-key: k", []string{"conclave-alert"}, 1)
	api.SeedIssue("Other App", "conclave-alert-key: k", []string{"conclave-alert"}, 999)
	closed, err := app.CreateAlertIssue(t.Context(), "Closed", "x", []string{"conclave-alert"})
	if err != nil {
		t.Fatal(err)
	}
	api.CloseIssue(closed.Number)
	if _, err := app.CreateAlertIssue(t.Context(), "Unlabelled", "x", []string{"other"}); err != nil {
		t.Fatal(err)
	}
	got, err := app.ListOpenAlertIssues(t.Context(), "conclave-alert")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != mine.Number || got[0].Body != "conclave-alert-key: k" {
		t.Fatalf("listed %+v, want only issue %d", got, mine.Number)
	}
}

func TestListOpenAlertIssuesPaginates(t *testing.T) {
	api, app := alertsFixture(t, alertsOptions())
	for i := range 150 {
		api.SeedIssue(fmt.Sprint("alert ", i), "b", []string{"conclave-alert"}, restBotID)
	}
	got, err := app.ListOpenAlertIssues(t.Context(), "conclave-alert")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 || got[0].Number != 1 || got[149].Number != 150 {
		t.Fatalf("listed %d issues, want 150 oldest first", len(got))
	}
}

// TestAlertCallsNeedAlertsConfig: with no alerts repository configured,
// as on the webhook server, every alert call is refused without a request.
func TestAlertCallsNeedAlertsConfig(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	calls := map[string]func() error{
		"create":   func() error { _, err := app.CreateAlertIssue(t.Context(), "t", "b", nil); return err },
		"list":     func() error { _, err := app.ListOpenAlertIssues(t.Context(), "conclave-alert"); return err },
		"comment":  func() error { _, err := app.CommentOnAlertIssue(t.Context(), 1, "b"); return err },
		"comments": func() error { _, err := app.AlertIssueComments(t.Context(), 1); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, github.ErrRefused) {
			t.Errorf("%s: got %v, want ErrRefused", name, err)
		}
	}
	if n := len(api.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestCommentOnAlertIssue(t *testing.T) {
	api, app := alertsFixture(t, alertsOptions())
	is, err := app.CreateAlertIssue(t.Context(), "t", "b", []string{"conclave-alert"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.CommentOnAlertIssue(t.Context(), is.Number, "again"); err != nil {
		t.Fatal(err)
	}
	got, err := app.AlertIssueComments(t.Context(), is.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "again" {
		t.Fatalf("comments %+v", got)
	}
	if reqs := api.RequestsTo("/repos/octo/ops/issues/1/comments"); len(reqs) != 2 {
		t.Fatalf("%d requests to the alert issue's comments, want 2", len(reqs))
	}
}

// TestAlertErrorsCarryNoSecrets is TestAPIErrorsCarryNoSecrets for the
// alert calls: failures that echo the credential leave none in any error.
func TestAlertErrorsCarryNoSecrets(t *testing.T) {
	calls := map[string]func(*github.App) error{
		"CreateAlertIssue": func(a *github.App) error {
			_, err := a.CreateAlertIssue(t.Context(), "t", "b", []string{"conclave-alert"})
			return err
		},
		"ListOpenAlertIssues": func(a *github.App) error {
			_, err := a.ListOpenAlertIssues(t.Context(), "conclave-alert")
			return err
		},
		"CommentOnAlertIssue": func(a *github.App) error {
			_, err := a.CommentOnAlertIssue(t.Context(), 1, "b")
			return err
		},
	}
	for name, call := range calls {
		for _, prefix := range []string{"/app/", "/repos/"} {
			for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
				t.Run(fmt.Sprintf("%s%s%d", name, prefix, status), func(t *testing.T) {
					opts := alertsOptions()
					opts.EchoAuthorization = true
					api, app := alertsFixture(t, opts)
					api.FailNext("", prefix, status, nil, "")
					err := call(app)
					if err == nil {
						t.Fatal("no error")
					}
					assertNoSecrets(t, err, observedCredentials(t, api)...)
				})
			}
		}
	}
}
