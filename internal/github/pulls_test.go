package github_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

const restPullsPath = "/repos/octo/tally/pulls"

func TestCreatePullRequestIdempotent(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	first, err := app.CreatePullRequest(t.Context(), restPull(restRepo))
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.CreatePullRequest(t.Context(), restPull(restRepo))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.Number != second.Number || first.NodeID == "" {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if n := countRequests(api, http.MethodPost, restPullsPath); n != 1 || api.Pulls() != 1 {
		t.Fatalf("%d creates sent, %d pull requests held; want 1 and 1", n, api.Pulls())
	}
	list := api.RequestsTo(restPullsPath)[0]
	if list.Method != http.MethodGet || list.RawQuery != "head=octo%3Aagent%2F7%2Fintegration&per_page=100&state=open" {
		t.Fatalf("first request %s ?%s", list.Method, list.RawQuery)
	}
}

// TestCreatePullRequestRace422 loses the create to a concurrent one, which
// GitHub answers with 422. The client looks again and adopts the winner if
// this App opened it.
func TestCreatePullRequestRace422(t *testing.T) {
	cases := []struct {
		name  string
		login string
		typ   string
		want  error
	}{
		{"won by this App", restSlug + "[bot]", "Bot", nil},
		{"won by someone else", "mallory", "User", github.ErrRefused},
		{"no pull request after all", "", "", github.ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			if tc.login != "" {
				api.RaceNextPullCreate(tc.login, tc.typ)
			} else {
				api.FailNext(http.MethodPost, restPullsPath, http.StatusUnprocessableEntity, nil, "")
			}
			pr, err := app.CreatePullRequest(t.Context(), restPull(restRepo))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.want == nil && (pr.Created || pr.Number != 1) {
				t.Fatalf("adopted %+v", pr)
			}
			if n := countRequests(api, http.MethodGet, restPullsPath); n != 2 {
				t.Fatalf("%d lists sent, want 2", n)
			}
		})
	}
}

func TestExistingPRByOtherAuthorNotAdopted(t *testing.T) {
	cases := []struct {
		name, login, typ, base string
	}{
		{"a person", "mallory", "User", "main"},
		{"another App", "other-app[bot]", "Bot", "main"},
		{"a user named like the bot", restSlug + "[bot]", "User", "main"},
		{"this App, to another base", restSlug + "[bot]", "Bot", "release"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			api.SeedPull("agent/7/integration", tc.base, tc.login, tc.typ)
			if _, err := app.CreatePullRequest(t.Context(), restPull(restRepo)); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if n := countRequests(api, http.MethodPost, restPullsPath); n != 0 {
				t.Fatalf("%d creates sent", n)
			}
		})
	}
}

func TestCreatePullRequestHeadMustBeAgentBranch(t *testing.T) {
	heads := []string{
		"main", "agent/1/integration:main", "+agent/1/pr-1", "refs/heads/agent/1/integration",
		"agent/0/integration", "agent/1/pr-1 ", "agent/../main", "agent/1/feature", "-d", "",
	}
	for _, head := range heads {
		t.Run(head, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			p := restPull(restRepo)
			p.Head = head
			if _, err := app.CreatePullRequest(t.Context(), p); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if n := len(api.Requests()); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}

func TestCreatePullRequestRejectsBadBase(t *testing.T) {
	cases := []struct {
		base, title string
		want        error
	}{
		{"", "t", github.ErrRefused},
		{"-main", "t", github.ErrRefused},
		{"main..x", "t", github.ErrRefused},
		{"../main", "t", github.ErrRefused},
		{"a b", "t", github.ErrRefused},
		{"main:x", "t", github.ErrRefused},
		{"~main", "t", github.ErrRefused},
		{"main\n", "t", github.ErrRefused},
		{strings.Repeat("a", 256), "t", github.ErrRefused},
		{"main", " ", github.ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.base+"/"+tc.title, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			p := restPull(restRepo)
			p.Base, p.Title = tc.base, tc.title
			if _, err := app.CreatePullRequest(t.Context(), p); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if n := len(api.Requests()); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}
