package github_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

func countRequests(api *githubtest.API, method, prefix string) int {
	n := 0
	for _, r := range api.RequestsTo(prefix) {
		if r.Method == method {
			n++
		}
	}
	return n
}

func TestEditCommentInPlace(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	c, err := app.CreateComment(t.Context(), restRepo, 7, "planning")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID <= 0 || !strings.Contains(c.URL, "issuecomment") {
		t.Fatalf("comment %+v", c)
	}
	for _, body := range []string{"implementing", "done"} {
		if err := app.EditComment(t.Context(), restRepo, c.ID, body); err != nil {
			t.Fatal(err)
		}
	}
	got := api.Comments()
	if len(got) != 1 || got[0].ID != c.ID || got[0].Body != "done" || got[0].AppID != restAppID {
		t.Fatalf("comments %+v, want one holding the last body", got)
	}
	if n := countRequests(api, http.MethodPatch, "/repos/"); n != 2 {
		t.Fatalf("%d edits sent, want 2", n)
	}
}

// TestEditCommentRefusesCommentNotAuthoredByApp gives EditComment the ID
// of a comment the owner, or another App, wrote. An installation token can
// edit it; the client must not.
func TestEditCommentRefusesCommentNotAuthoredByApp(t *testing.T) {
	for name, appID := range map[string]int64{"owner's comment": 0, "another App's comment": 999} {
		t.Run(name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			id := api.SeedComment(7, "/approve", appID)
			if err := app.EditComment(t.Context(), restRepo, id, "rewritten"); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if n := countRequests(api, http.MethodPatch, "/repos/"); n != 0 {
				t.Fatalf("%d edits sent", n)
			}
			if got := api.Comments(); got[0].Body != "/approve" {
				t.Fatalf("comment rewritten to %q", got[0].Body)
			}
		})
	}
}

// TestListIssueCommentsKeepsOnlyAppComments seeds a thread longer than
// one page, mixing this App's comments with the owner's, another App's,
// and comments on another issue. Only this App's comments on the issue
// come back, in order, with their bodies, across the page boundary.
func TestListIssueCommentsKeepsOnlyAppComments(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	var want []int64
	for i := range 150 {
		switch i % 3 {
		case 0:
			want = append(want, api.SeedComment(7, "platform "+strconv.Itoa(i), restAppID))
		case 1:
			api.SeedComment(7, "platform marker quoted by the owner "+strconv.Itoa(i), 0)
		default:
			api.SeedComment(7, "platform marker from another App", 999)
		}
	}
	api.SeedComment(8, "platform on another issue", restAppID)

	got, err := app.ListIssueComments(t.Context(), restRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d comments, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.ID != want[i] || !strings.HasPrefix(c.Body, "platform ") || strings.Contains(c.Body, "quoted") {
			t.Fatalf("comment %d: %+v, want ID %d with its body", i, c, want[i])
		}
	}
	if n := countRequests(api, http.MethodGet, "/repos/octo/tally/issues/7/comments"); n != 2 {
		t.Fatalf("%d list requests, want 2 pages", n)
	}
}

func TestListIssueCommentsRefusesBadInput(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	if _, err := app.ListIssueComments(t.Context(), restRepo, 0); !errors.Is(err, github.ErrRefused) {
		t.Fatalf("issue 0: got %v, want ErrRefused", err)
	}
	bad := restRepo
	bad.FullName = "../etc"
	if _, err := app.ListIssueComments(t.Context(), bad, 7); err == nil {
		t.Fatal("bad full name accepted")
	}
	if n := len(api.RequestsTo("/repos/")); n != 0 {
		t.Fatalf("%d repository requests sent", n)
	}
}

func TestAddReactionAllowlist(t *testing.T) {
	allowed := []github.Reaction{
		github.ReactionPlusOne, github.ReactionMinusOne, github.ReactionEyes,
		github.ReactionConfused, github.ReactionRocket, github.ReactionHooray,
	}
	t.Run("allowed and idempotent", func(t *testing.T) {
		api, app := restFixture(t, restOptions(), nil)
		for _, r := range allowed {
			for _, target := range []github.ReactionTarget{{Issue: 7}, {CommentID: 1000}, {Issue: 7}} {
				if err := app.AddReaction(t.Context(), restRepo, target, r); err != nil {
					t.Fatalf("%s on %+v: %v", r, target, err)
				}
			}
		}
		if n := countRequests(api, http.MethodPost, "/repos/octo/tally/issues/comments/1000/reactions"); n != len(allowed) {
			t.Fatalf("%d comment reactions sent, want %d", n, len(allowed))
		}
	})
	refused := []struct {
		name     string
		target   github.ReactionTarget
		reaction github.Reaction
	}{
		{"heart", github.ReactionTarget{Issue: 7}, "heart"},
		{"laugh", github.ReactionTarget{Issue: 7}, "laugh"},
		{"arbitrary", github.ReactionTarget{Issue: 7}, "canary_reaction"},
		{"padded", github.ReactionTarget{Issue: 7}, "+1 "},
		{"neither target", github.ReactionTarget{}, github.ReactionEyes},
		{"both targets", github.ReactionTarget{Issue: 7, CommentID: 1000}, github.ReactionEyes},
		{"negative issue", github.ReactionTarget{Issue: -7}, github.ReactionEyes},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			if err := app.AddReaction(t.Context(), restRepo, tc.target, tc.reaction); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if n := len(api.Requests()); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}

// TestOutgoingBodyCarryingTokenRefused tries to publish the live token, and
// strings shaped like other credentials, through every method that sends
// text.
func TestOutgoingBodyCarryingTokenRefused(t *testing.T) {
	bodies := map[string]string{
		"live token":      "debug: " + githubtest.CanaryToken,
		"installation":    "token " + "gh" + "s_" + strings.Repeat("A1b2", 9),
		"personal access": "github" + "_pat_" + strings.Repeat("Z9", 20),
		"JWT":             "eyJ" + "hbGciOiJSUzI1NiJ9.eyJpc3MiOiI0MiJ9.c2lnbmF0dXJlLXZhbHVl",
	}
	for name, body := range bodies {
		for _, method := range []string{"CreateComment", "EditComment", "PR title", "PR body"} {
			t.Run(name+"/"+method, func(t *testing.T) {
				api, app := restFixture(t, restOptions(), nil)
				var err error
				pr := restPull(restRepo)
				switch method {
				case "CreateComment":
					_, err = app.CreateComment(t.Context(), restRepo, 7, body)
				case "EditComment":
					err = app.EditComment(t.Context(), restRepo, api.SeedComment(7, "old", restAppID), body)
				case "PR title":
					pr.Title = body
					_, err = app.CreatePullRequest(t.Context(), pr)
				case "PR body":
					pr.Body = body
					_, err = app.CreatePullRequest(t.Context(), pr)
				}
				if !errors.Is(err, github.ErrRefused) {
					t.Fatalf("got %v, want ErrRefused", err)
				}
				if n := len(api.RequestsTo("/repos/")); n != 0 {
					t.Fatalf("%d repository requests sent", n)
				}
			})
		}
	}
}

func TestCommentTooLong(t *testing.T) {
	// Characters, not bytes, are counted: each é is two bytes.
	limit := strings.Repeat("é", 65536)
	api, app := restFixture(t, restOptions(), nil)
	if _, err := app.CreateComment(t.Context(), restRepo, 7, limit); err != nil {
		t.Fatalf("a body at the limit: %v", err)
	}
	sent := len(api.RequestsTo("/repos/"))
	for _, body := range []string{limit + "é", ""} {
		if _, err := app.CreateComment(t.Context(), restRepo, 7, body); !errors.Is(err, github.ErrInvalidRequest) {
			t.Fatalf("body of %d bytes: got %v, want ErrInvalidRequest", len(body), err)
		}
		if err := app.EditComment(t.Context(), restRepo, 1000, body); !errors.Is(err, github.ErrInvalidRequest) {
			t.Fatalf("edit to %d bytes: got %v, want ErrInvalidRequest", len(body), err)
		}
	}
	if n := len(api.RequestsTo("/repos/")); n != sent {
		t.Fatalf("%d requests sent for refused bodies", n-sent)
	}
}

// TestRepoNameValidated hands every REST method full names that would steer
// a request path elsewhere.
func TestRepoNameValidated(t *testing.T) {
	names := []string{
		"a/b/../../x", "a/b?x", "a", "a/b/c", "a/.git", "a/x.git", "-a/b", "a/..", "a/.",
		"a/b#x", "a/b%2e", "", "a/b ", "a/b\n", "a@b/c", "../a/b", "/a/b", "a//b",
	}
	for _, name := range names {
		for _, c := range restCalls {
			t.Run(c.name+"/"+name, func(t *testing.T) {
				api, app := restFixture(t, restOptions(), nil)
				repo := github.RepoRef{ID: 101, FullName: name, InstallationID: restInstallationID}
				if err := c.call(t.Context(), app, repo); !errors.Is(err, github.ErrRefused) {
					t.Fatalf("got %v, want ErrRefused", err)
				}
				if n := len(api.Requests()); n != 0 {
					t.Fatalf("%d requests sent", n)
				}
			})
		}
	}
}
