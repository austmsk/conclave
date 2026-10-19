package github_test

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

// activityStarter stands in for ticket 8's workflow: on a start it does
// what the first activities will do with the App, against a fake GitHub
// that echoes every credential back in its failures, and logs each error
// the way an activity's logger will. It records the errors so the test can
// inspect them too.
type activityStarter struct {
	app *github.App
	log *slog.Logger

	mu   sync.Mutex
	errs []error
}

func (s *activityStarter) StartFeature(ctx context.Context, req github.FeatureStart) error {
	s.log.Info("feature start", "workflow_id", req.WorkflowID, "request", req)
	issue := req.Issue.Number
	c, err := s.app.CreateComment(ctx, req.Repo, issue, "Working on it.")
	s.note("create comment", err)
	s.note("add reaction", s.app.AddReaction(ctx, req.Repo, github.ReactionTarget{Issue: issue}, github.ReactionEyes))
	if c.ID != 0 {
		s.note("edit comment", s.app.EditComment(ctx, req.Repo, c.ID, "Still working."))
	}
	_, err = s.app.CreatePullRequest(ctx, github.PullRequestSpec{
		Repo: req.Repo, Head: "agent/7/integration", Base: "main", Title: "Fix #7", Body: "Closes #7",
	})
	s.note("create pull request", err)
	_, err = s.app.CreateComment(ctx, req.Repo, issue, "Done.")
	s.note("create comment", err)
	return nil
}

func (s *activityStarter) SignalCommand(_ context.Context, req github.CommandSignal) error {
	s.log.Info("command", "workflow_id", req.WorkflowID, "request", req)
	return nil
}

func (s *activityStarter) note(op string, err error) {
	if err == nil {
		return
	}
	s.log.Error("activity failed", "op", op, "err", err)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}

// TestNoTokenInLogsOrErrors drives the whole path, deliveries through the
// receiver and dispatcher into App calls, with GitHub failing those calls
// and echoing the credential it was sent into its bodies, headers, and
// request IDs. The log is written by a plain JSON handler, without
// RedactAttr, so nothing is scrubbed after the fact: no token, App JWT,
// Basic credential, or webhook secret may appear in it or in any error.
func TestNoTokenInLogsOrErrors(t *testing.T) {
	api := githubtest.NewAPI(t, githubtest.APIOptions{
		AppID: githubtest.FixtureAppID, InstallationID: githubtest.FixtureInstallationID,
		AppSlug: githubtest.FixtureAppSlug, EchoAuthorization: true,
	})
	for _, f := range []struct {
		method string
		status int
	}{
		{http.MethodPost, http.StatusUnauthorized},
		{http.MethodGet, http.StatusUnprocessableEntity},
		{http.MethodPost, http.StatusInternalServerError},
		{http.MethodPost, http.StatusTemporaryRedirect},
	} {
		api.FailNext(f.method, "/repos/", f.status, map[string]string{"Location": api.URL + "/elsewhere"}, "")
	}
	cfg := githubtest.WebhookConfig()
	cfg.APIBaseURL, cfg.GitBaseURL = api.URL, api.URL
	app, err := github.NewApp(cfg, githubtest.Key(t), nil)
	if err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	starter := &activityStarter{app: app, log: log}
	disp := github.NewDispatcher(starter, log, 8)
	recv, err := github.NewReceiver(cfg, githubtest.EnabledRepos(), []byte(secret), disp, log)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	go func() { defer close(ran); disp.Run(context.Background()) }()

	deliver(t, recv, "issues", githubtest.LabeledEvent().JSON(), true)
	deliver(t, recv, "issue_comment", githubtest.CommentEvent("/approve").JSON(), true)
	deliver(t, recv, "issues", githubtest.LabeledEvent().JSON(), false)
	disp.Close(30 * time.Second)
	<-ran

	creds := leakCredentials(t, api)
	if len(starter.errs) == 0 {
		t.Fatal("no App call failed; the injected failures were not exercised")
	}
	for _, err := range starter.errs {
		assertNoSecrets(t, err, creds...)
	}
	out := logs.String()
	for _, c := range creds {
		if strings.Contains(out, c) {
			t.Fatalf("log carries a credential:\n%s", out)
		}
	}
	if !strings.Contains(out, "activity failed") || !strings.Contains(out, "feature start") {
		t.Fatalf("log is missing the activity lines it should have:\n%s", out)
	}
}

// deliver sends one delivery, signed correctly when signed is true and
// with a forged signature otherwise.
func deliver(t *testing.T, recv *github.Receiver, event string, body []byte, signed bool) {
	t.Helper()
	req := githubtest.Delivery(t, secret, event, body)
	if !signed {
		req.Header.Set(sigHeader, githubtest.Sign([]byte("canary_wrong_secret_wrong_secret_0123"), body))
	}
	rec := httptest.NewRecorder()
	recv.ServeHTTP(rec, req)
	if want := map[bool]int{true: http.StatusAccepted, false: http.StatusUnauthorized}[signed]; rec.Code != want {
		t.Fatalf("%s delivery (signed %v): status %d, want %d", event, signed, rec.Code, want)
	}
}

// leakCredentials is every credential the run used: the installation
// token, its Basic form, each App JWT the fake saw, and the webhook secret.
// It also proves the token was obtained and used: a repository call
// carried it.
func leakCredentials(t *testing.T, api *githubtest.API) []string {
	t.Helper()
	used := false
	for _, r := range api.RequestsTo("/repos/") {
		used = used || r.Authorization() == "Bearer "+githubtest.CanaryToken
	}
	if api.Mints() == 0 || !used {
		t.Fatalf("installation token minted %d times, used on a repository call: %v", api.Mints(), used)
	}
	creds := observedCredentials(t, api)
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubtest.CanaryToken))
	return append(creds, basic, githubtest.CanaryWebhookSecret)
}
