package github_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

// Shared fixtures for the REST client tests. Names carry "rest" so they do
// not collide with the webhook and push tests in this package.

const (
	restAppID          = 4242
	restInstallationID = 77
	restSlug           = "conclave-test"
)

var restRepo = github.RepoRef{ID: 101, FullName: "octo/tally", InstallationID: restInstallationID}

func restOptions() githubtest.APIOptions {
	return githubtest.APIOptions{AppID: restAppID, InstallationID: restInstallationID, AppSlug: restSlug}
}

func restConfig(base string) github.Config {
	return github.Config{
		AppID: restAppID, AppSlug: restSlug, InstallationID: restInstallationID, OwnerUserID: 1, BotUserID: 2,
		TriggerLabel: "agent", APIBaseURL: base, GitBaseURL: base,
	}
}

// restFixture starts a fake API with opts and returns it with an App that
// talks to it, using hc as the App's HTTP client.
func restFixture(t *testing.T, opts githubtest.APIOptions, hc *http.Client) (*githubtest.API, *github.App) {
	t.Helper()
	api := githubtest.NewAPI(t, opts)
	app, err := github.NewApp(restConfig(api.URL), githubtest.Key(t), hc)
	if err != nil {
		t.Fatal(err)
	}
	return api, app
}

func restPull(repo github.RepoRef) github.PullRequestSpec {
	return github.PullRequestSpec{Repo: repo, Head: "agent/7/integration", Base: "main", Title: "Fix #7", Body: "Closes #7"}
}

// restCall is one Client method that talks to the REST API.
type restCall struct {
	name string
	call func(ctx context.Context, a *github.App, repo github.RepoRef) error
}

var restCalls = []restCall{
	{"CreateComment", func(ctx context.Context, a *github.App, repo github.RepoRef) error {
		_, err := a.CreateComment(ctx, repo, 7, "status")
		return err
	}},
	{"EditComment", func(ctx context.Context, a *github.App, repo github.RepoRef) error {
		return a.EditComment(ctx, repo, 1000, "status")
	}},
	{"AddReaction", func(ctx context.Context, a *github.App, repo github.RepoRef) error {
		return a.AddReaction(ctx, repo, github.ReactionTarget{Issue: 7}, github.ReactionEyes)
	}},
	{"CreatePullRequest", func(ctx context.Context, a *github.App, repo github.RepoRef) error {
		_, err := a.CreatePullRequest(ctx, restPull(repo))
		return err
	}},
}

// restClock is a clock tests move by hand, shared by the App and the fake.
type restClock struct {
	mu  sync.Mutex
	now time.Time
}

func newRESTClock() *restClock {
	return &restClock{now: time.Now().Truncate(time.Second)}
}

func (c *restClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *restClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decoding %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		want    error
		retry   time.Duration
	}{
		{"unauthorized", http.StatusUnauthorized, nil, github.ErrAuth, 0},
		{"primary rate limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, github.ErrRateLimited, 0},
		{"secondary rate limit", http.StatusForbidden, map[string]string{"Retry-After": "30"}, github.ErrRateLimited, 30 * time.Second},
		{"forbidden", http.StatusForbidden, nil, github.ErrInvalidRequest, 0},
		{"not found", http.StatusNotFound, nil, github.ErrNotFound, 0},
		{"request timeout", http.StatusRequestTimeout, nil, github.ErrUnavailable, 0},
		{"unprocessable", http.StatusUnprocessableEntity, nil, github.ErrInvalidRequest, 0},
		{"too many requests", http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, github.ErrRateLimited, 7 * time.Second},
		{"internal error", http.StatusInternalServerError, nil, github.ErrUnavailable, 0},
		{"unavailable", http.StatusServiceUnavailable, nil, github.ErrUnavailable, 0},
		{"redirect", http.StatusTemporaryRedirect, map[string]string{"Location": "https://elsewhere.example/"}, github.ErrInvalidRequest, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			headers := map[string]string{"X-GitHub-Request-Id": "ABCD:12EF"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			api.FailNext("", "/repos/", tc.status, headers, `{"message":"no"}`)
			_, err := app.CreateComment(t.Context(), restRepo, 7, "status")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			var apiErr *github.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("%v is not an *APIError", err)
			}
			if apiErr.Status != tc.status || apiErr.RetryAfter != tc.retry || apiErr.RequestID != "ABCD:12EF" {
				t.Fatalf("APIError %+v", *apiErr)
			}
		})
	}
}

// TestClientRefusesRedirect attempts to steal the JWT and the installation
// token with a redirect to another server. The client must not follow it,
// whether NewApp built the HTTP client or the caller supplied it.
func TestClientRefusesRedirect(t *testing.T) {
	clients := map[string]*http.Client{"default client": nil, "caller's client": {Timeout: time.Minute}}
	for _, prefix := range []string{"/app/", "/repos/"} {
		for name, hc := range clients {
			t.Run(prefix+"/"+name, func(t *testing.T) {
				var stolen atomic.Int32
				attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { stolen.Add(1) }))
				t.Cleanup(attacker.Close)
				api, app := restFixture(t, restOptions(), hc)
				api.FailNext("", prefix, http.StatusTemporaryRedirect, map[string]string{"Location": attacker.URL + "/steal"}, "")

				_, err := app.CreateComment(t.Context(), restRepo, 7, "status")
				if !errors.Is(err, github.ErrInvalidRequest) {
					t.Fatalf("got %v, want ErrInvalidRequest", err)
				}
				if stolen.Load() != 0 {
					t.Fatalf("the redirect target received %d requests", stolen.Load())
				}
			})
		}
	}
}

func TestNewAppRejects(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	invalid := restConfig("https://api.github.com")
	invalid.AppID = 0
	cases := []struct {
		name string
		cfg  github.Config
		key  *rsa.PrivateKey
		hc   *http.Client
	}{
		{"nil key", restConfig("https://api.github.com"), nil, nil},
		{"1024-bit key", restConfig("https://api.github.com"), small, nil},
		{"client without timeout", restConfig("https://api.github.com"), githubtest.Key(t), &http.Client{}},
		{"invalid config", invalid, githubtest.Key(t), nil},
		{"plain http to a remote host", restConfig("http://api.github.com"), githubtest.Key(t), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := github.NewApp(tc.cfg, tc.key, tc.hc); !errors.Is(err, github.ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestAPIErrorsCarryNoSecrets runs every REST method against failures
// whose bodies and headers echo the credential the request carried: the
// JWT when the token exchange fails, the installation token when the
// repository call does. No error in the chain may carry either.
func TestAPIErrorsCarryNoSecrets(t *testing.T) {
	statuses := []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity,
		http.StatusInternalServerError, http.StatusTemporaryRedirect,
	}
	for _, c := range restCalls {
		for _, prefix := range []string{"/app/", "/repos/"} {
			for _, status := range statuses {
				t.Run(fmt.Sprintf("%s%s%d", c.name, prefix, status), func(t *testing.T) {
					opts := restOptions()
					opts.EchoAuthorization = true
					api, app := restFixture(t, opts, nil)
					api.FailNext("", prefix, status, map[string]string{"Location": "https://elsewhere.example/"}, "")
					err := c.call(t.Context(), app, restRepo)
					if err == nil {
						t.Fatal("no error")
					}
					assertNoSecrets(t, err, observedCredentials(t, api)...)
				})
			}
		}
	}
}

// observedCredentials returns the installation token and every JWT the
// fake saw.
func observedCredentials(t *testing.T, api *githubtest.API) []string {
	t.Helper()
	creds := []string{githubtest.CanaryToken}
	for _, r := range api.RequestsTo("/app/") {
		jwt, ok := strings.CutPrefix(r.Authorization(), "Bearer ")
		if !ok || len(jwt) < 32 {
			t.Fatalf("token exchange sent Authorization %q", r.Authorization())
		}
		creds = append(creds, jwt)
	}
	return creds
}

// assertNoSecrets checks the text and the formatted forms of err and of
// every error it wraps.
func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		for _, text := range []string{e.Error(), fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e)} {
			for _, s := range secrets {
				if strings.Contains(text, s) {
					t.Fatalf("error %q carries a credential", text)
				}
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			if next := u.Unwrap(); next != nil {
				queue = append(queue, next)
			}
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
}

func TestSecretHoldersFormatRedacted(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	if _, err := app.CreateComment(t.Context(), restRepo, 7, "status"); err != nil {
		t.Fatal(err)
	}
	keyDigits := githubtest.Key(t).D.String()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, app)
		if strings.Contains(out, githubtest.CanaryToken) || strings.Contains(out, keyDigits[:32]) || !strings.Contains(out, "[redacted]") {
			t.Errorf("App formatted with %s: %s", verb, out)
		}
	}
	if api.Mints() != 1 {
		t.Fatalf("mints %d, want 1", api.Mints())
	}

	const value = "canary_secret_holder_value"
	s := github.NewSecret(value)
	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		outputs = append(outputs, fmt.Sprintf(verb, s), fmt.Sprintf(verb, struct{ S any }{s}))
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("holding", "secret", s, "nested", struct{ S any }{s})
	marshalled, err := json.Marshal(struct{ S any }{s})
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, logged.String(), string(marshalled))
	for _, out := range outputs {
		if strings.Contains(out, value) || !strings.Contains(out, "[redacted]") {
			t.Errorf("secret printed as %s", out)
		}
	}
}

func TestTransportFailureIsUnavailable(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	app, err := github.NewApp(restConfig(closed.URL), githubtest.Key(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	github.StubToken(app, githubtest.CanaryToken)

	_, err = app.CreateComment(t.Context(), restRepo, 7, "status")
	var apiErr *github.APIError
	if !errors.Is(err, github.ErrUnavailable) || !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an *APIError for ErrUnavailable", err)
	}
	assertNoSecrets(t, err, githubtest.CanaryToken, closed.URL)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = app.CreateComment(ctx, restRepo, 7, "status")
	if !errors.Is(err, context.Canceled) || errors.Is(err, github.ErrUnavailable) {
		t.Fatalf("cancelled call: got %v, want context.Canceled only", err)
	}
}

func TestIsPermanent(t *testing.T) {
	apiErr := func(status int) error {
		api, app := restFixture(t, restOptions(), nil)
		api.FailNext("", "/repos/", status, nil, "")
		_, err := app.CreateComment(t.Context(), restRepo, 7, "status")
		return err
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"refused", fmt.Errorf("wrapped: %w", github.ErrRefused), true},
		{"invalid config", github.ErrInvalidConfig, true},
		{"422", apiErr(http.StatusUnprocessableEntity), true},
		{"404", apiErr(http.StatusNotFound), true},
		{"401", apiErr(http.StatusUnauthorized), false},
		{"429", apiErr(http.StatusTooManyRequests), false},
		{"503", apiErr(http.StatusServiceUnavailable), false},
		{"cancelled", context.Canceled, false},
		{"non-fast-forward push", &github.PushError{Reason: github.PushNonFastForward}, true},
		{"stale lease push", &github.PushError{Reason: github.PushStaleLease}, true},
		{"remote rejected push", fmt.Errorf("pushing: %w", &github.PushError{Reason: github.PushRemoteRejected}), true},
		{"push auth", &github.PushError{Reason: github.PushAuth}, false},
		{"push other", &github.PushError{Reason: github.PushOther}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := github.IsPermanent(tc.err); got != tc.want {
				t.Fatalf("IsPermanent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
