package github_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

const restCommit = "0123456789abcdef0123456789abcdef01234567"

func TestListCheckRuns(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	build := api.SeedCheckRun(restCommit, "build", "completed", "success", "github-actions")
	api.SeedCheckRun(restCommit, "lint", "in_progress", "", "github-actions")
	api.SeedCheckRun("fedcba9876543210fedcba9876543210fedcba98", "other", "completed", "failure", "github-actions")
	runs, err := app.ListCheckRuns(t.Context(), restRepo, restCommit)
	if err != nil {
		t.Fatal(err)
	}
	want := []github.CheckRun{
		{ID: build, Name: "build", Status: "completed", Conclusion: "success", AppSlug: "github-actions"},
		{ID: build + 1, Name: "lint", Status: "in_progress", AppSlug: "github-actions"},
	}
	if fmt.Sprint(runs) != fmt.Sprint(want) {
		t.Fatalf("ListCheckRuns = %+v, want %+v", runs, want)
	}
	req := api.RequestsTo("/repos/octo/tally/commits/")[0]
	if req.Path != "/repos/octo/tally/commits/"+restCommit+"/check-runs" || req.RawQuery != "page=1&per_page=100" {
		t.Fatalf("request %s?%s", req.Path, req.RawQuery)
	}
}

// TestListCheckRunsPaginates reads every page up to total_count.
func TestListCheckRunsPaginates(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	for i := range 230 {
		api.SeedCheckRun(restCommit, fmt.Sprintf("check-%d", i), "completed", "success", "github-actions")
	}
	runs, err := app.ListCheckRuns(t.Context(), restRepo, restCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 230 || runs[229].Name != "check-229" {
		t.Fatalf("got %d runs", len(runs))
	}
	if n := countRequests(api, http.MethodGet, "/repos/octo/tally/commits/"); n != 3 {
		t.Fatalf("%d pages read, want 3", n)
	}
}

// TestListCheckRunsRefusesNonCommit: a ref that is not a full commit ID
// is refused before a token is minted or a request sent, so a branch name
// or a path cannot steer the read.
func TestListCheckRunsRefusesNonCommit(t *testing.T) {
	for _, ref := range []string{"main", "0123456", "HEAD", "../pulls", restCommit + "/../x", strings.ToUpper(restCommit), ""} {
		t.Run(ref, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			if _, err := app.ListCheckRuns(t.Context(), restRepo, ref); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if n := len(api.Requests()); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}

func TestMarkPullRequestReadyOnce(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	n := api.SeedDraftPull("agent/7/pr-1", "main", restSlug+"[bot]", "Bot")
	for i := range 2 {
		pr, err := app.MarkPullRequestReady(t.Context(), restRepo, n)
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if pr.Draft || pr.Number != n || pr.NodeID == "" {
			t.Fatalf("call %d: %+v", i+1, pr)
		}
	}
	if api.PullDraft(n) {
		t.Fatal("pull request is still a draft")
	}
	if got := countRequests(api, http.MethodPost, "/graphql"); got != 1 {
		t.Fatalf("%d mutations sent, want 1: a retry must not mark it again", got)
	}
}

// TestMarkPullRequestReadyRefusesOthersWork: the platform marks ready only
// a pull request its own bot opened from an agent/ branch. A pull request
// by a person, by another App, or from another head gets no mutation.
func TestMarkPullRequestReadyRefusesOthersWork(t *testing.T) {
	cases := []struct{ name, head, login, typ string }{
		{"a person", "agent/7/pr-1", "mallory", "User"},
		{"another App", "agent/7/pr-1", "other-app[bot]", "Bot"},
		{"a user named like the bot", "agent/7/pr-1", restSlug + "[bot]", "User"},
		{"a head outside agent/", "feature/x", restSlug + "[bot]", "Bot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			n := api.SeedDraftPull(tc.head, "main", tc.login, tc.typ)
			if _, err := app.MarkPullRequestReady(t.Context(), restRepo, n); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if !api.PullDraft(n) || countRequests(api, http.MethodPost, "/graphql") != 0 {
				t.Fatal("a mutation was sent for a pull request the App does not own")
			}
		})
	}
}

// TestMarkPullRequestReadyGraphQLError: GitHub's GraphQL errors arrive on
// a 200 and quote the request. The error is ErrInvalidRequest with fixed
// text, carrying neither GitHub's message nor the token.
func TestMarkPullRequestReadyGraphQLError(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	n := api.SeedDraftPull("agent/7/pr-1", "main", restSlug+"[bot]", "Bot")
	const leak = "resource not accessible: " + githubtest.CanaryToken
	api.FailNextGraphQL(leak)
	_, err := app.MarkPullRequestReady(t.Context(), restRepo, n)
	if !errors.Is(err, github.ErrInvalidRequest) {
		t.Fatalf("got %v, want ErrInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "resource not accessible") || strings.Contains(err.Error(), githubtest.CanaryToken) {
		t.Fatalf("error quotes GitHub's reply: %v", err)
	}
	if !api.PullDraft(n) {
		t.Fatal("pull request left draft despite the error")
	}
}

func TestMarkPullRequestReadyMissing(t *testing.T) {
	_, app := restFixture(t, restOptions(), nil)
	if _, err := app.MarkPullRequestReady(t.Context(), restRepo, 9); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if _, err := app.MarkPullRequestReady(t.Context(), restRepo, 0); !errors.Is(err, github.ErrRefused) {
		t.Fatalf("number 0: got %v, want ErrRefused", err)
	}
}
