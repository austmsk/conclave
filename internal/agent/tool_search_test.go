package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/sandbox"
)

// searchTree is the workspace the search tests run over.
var searchTree = map[string]string{
	"README.md":           "needle in the readme\n",
	"src/main.go":         "package main\n// needle here\n",
	"src/util/strings.go": "no match\nneedle twice needle\n",
	"docs/guide.txt":      "haystack\n",
}

func runSearch(t *testing.T, sb *agenttest.Sandbox, denylist []string, args map[string]any) (string, error) {
	t.Helper()
	res, err := newSearch(fileToolEnv(denylist...)).Execute(context.Background(), sb, toolArgs(t, args))
	return res.Output, err
}

func TestSearchFindsMatches(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"whole workspace", map[string]any{"pattern": "needle"}, "README.md:1:needle in the readme\nsrc/main.go:2:// needle here\nsrc/util/strings.go:2:needle twice needle\n"},
		{"root dot", map[string]any{"pattern": "readme", "path": "."}, "README.md:1:needle in the readme\n"},
		{"directory", map[string]any{"pattern": "needle", "path": "src/util"}, "src/util/strings.go:2:needle twice needle\n"},
		{"single file", map[string]any{"pattern": "needle", "path": "src/main.go"}, "src/main.go:2:// needle here\n"},
		{"extended regex", map[string]any{"pattern": "^(no|hay)", "path": "src"}, "src/util/strings.go:1:no match\n"},
		{"glob on name", map[string]any{"pattern": "needle", "glob": "*.go"}, "src/main.go:2:// needle here\nsrc/util/strings.go:2:needle twice needle\n"},
		{"glob on path", map[string]any{"pattern": "needle", "glob": "src/*.go"}, "src/main.go:2:// needle here\n"},
		{"no matches", map[string]any{"pattern": "absent"}, "no matches\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			execOnHost(sb)
			seed(t, sb.Dir, searchTree)
			got, err := runSearch(t, sb, nil, tc.args)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if sorted(got) != sorted(tc.want) {
				t.Fatalf("search = %q, want %q", got, tc.want)
			}
		})
	}
}

// sorted orders output lines, since find's order is the filesystem's.
func sorted(s string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestSearchRefusals(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"missing pattern", map[string]any{"path": "src"}, codeBadArgs},
		{"nul in pattern", map[string]any{"pattern": "a\x00b"}, codeBadArgs},
		{"long pattern", map[string]any{"pattern": strings.Repeat("a", maxPatternBytes+1)}, codeBadArgs},
		{"bad glob", map[string]any{"pattern": "a", "glob": "[x"}, codeBadArgs},
		{"unknown field", map[string]any{"pattern": "a", "flags": "-r"}, codeBadArgs},
		{"bad regex", map[string]any{"pattern": "(unclosed"}, codeBadPattern},
		{"missing path", map[string]any{"pattern": "a", "path": "nope"}, codeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			execOnHost(sb)
			seed(t, sb.Dir, searchTree)
			_, err := runSearch(t, sb, nil, tc.args)
			var r *Refusal
			if !errors.As(err, &r) || r.Code != tc.code {
				t.Fatalf("search = %v, want refusal %s", err, tc.code)
			}
		})
	}
}

// TestSearchArgumentInjection proves the model's path and pattern reach
// grep as data: a path or pattern that looks like an option is searched
// for, not obeyed.
func TestSearchArgumentInjection(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"path is -f", map[string]any{"pattern": "needle", "path": "-f"}, "-f:1:needle in a dash file\n"},
		{"path is -r dir", map[string]any{"pattern": "needle", "path": "-r"}, "-r/x.txt:1:needle in a dash dir\n"},
		{"pattern is -f file", map[string]any{"pattern": "-f/etc/passwd"}, "no matches\n"},
		{"pattern is --help", map[string]any{"pattern": "--help", "path": "opts.txt"}, "opts.txt:1:run with --help\n"},
		{"pattern is -v", map[string]any{"pattern": "-v", "path": "opts.txt"}, "opts.txt:2:or -v for more\n"},
		{"pattern is -e", map[string]any{"pattern": "-e", "path": "opts.txt"}, "no matches\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			execOnHost(sb)
			seed(t, sb.Dir, map[string]string{
				"-f":       "needle in a dash file\n",
				"-r/x.txt": "needle in a dash dir\n",
				"opts.txt": "run with --help\nor -v for more\n",
			})
			got, err := runSearch(t, sb, nil, tc.args)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if got != tc.want {
				t.Fatalf("search = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchExcludesGitAndDenylist(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	execOnHost(sb)
	seed(t, sb.Dir, map[string]string{
		".git/config":             "needle canary_git\n",
		"vendor/lib/.git/HEAD":    "needle canary_nested_git\n",
		"git~1/HEAD":              "needle canary_short_name\n",
		"secrets/key.pem":         "needle canary_secret\n",
		"app/SECRETS/key.pem":     "needle canary_upper_secret\n",
		"config/prod.yaml":        "needle canary_prod\n",
		"deploy/config/prod.yaml": "needle canary_nested_prod\n",
		"config/dev.yaml":         "needle dev\n",
		"secretsfoo/ok.txt":       "needle ok\n",
	})
	denylist := []string{"secrets", "config/prod.yaml"}
	got, err := runSearch(t, sb, denylist, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if want := "config/dev.yaml:1:needle dev\nsecretsfoo/ok.txt:1:needle ok"; sorted(got) != want {
		t.Errorf("search = %q, want %q", got, want)
	}
	if strings.Contains(got, "canary_") {
		t.Errorf("a hidden file's content leaked: %q", got)
	}
	for _, p := range []string{".git", "secrets", "app/Secrets", "config/prod.yaml", "git~1"} {
		if _, err := runSearch(t, sb, denylist, map[string]any{"pattern": "needle", "path": p}); !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("search in %q = %v, want ErrInvalidPath", p, err)
		}
	}
}

// TestFormatMatchesFailsClosed feeds the parser lines grep could print
// for hostile file names and checks that nothing that might come from a
// hidden file survives.
func TestFormatMatchesFailsClosed(t *testing.T) {
	lines := []string{
		"./src/ok.go:3:needle a:12:b",                // kept: ":12:" is in the text
		"./.git/config:1:needle",                     // .git
		"./.GIT/config:1:needle",                     // .git, folded
		"./a/git~1/HEAD:1:needle",                    // NTFS short name
		"./secrets/k:1:needle",                       // denylisted
		"./a:1:/secrets/k.txt:5:needle",              // a file "a:1:" in a dir: the other reading is denylisted
		"secrets/k:1:needle",                         // no "./": the tail of a name holding a newline
		"./src/x\U0000202e.go:1:needle",              // a name CheckPath refuses
		"./no line number here",                      // unparseable
		"./:1:needle",                                // empty path
		"./bin/blob.bin:2:needle \xff\xfe raw bytes", // kept, text replaced
	}
	got := formatMatches([]byte(strings.Join(lines, "\n")+"\n"), "", []string{"secrets"})
	want := "src/ok.go:3:needle a:12:b\nbin/blob.bin:2:[binary content omitted]\n"
	if got != want {
		t.Fatalf("formatMatches = %q, want %q", got, want)
	}
}

func TestFormatMatchesTruncatesWithCount(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= maxSearchMatches+7; i++ {
		fmt.Fprintf(&b, "./f.txt:%d:needle\n", i)
	}
	got := formatMatches([]byte(b.String()), "", nil)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != maxSearchMatches+1 {
		t.Fatalf("got %d lines, want %d matches and a count", len(lines), maxSearchMatches)
	}
	if last := lines[len(lines)-1]; last != "... 7 more matches truncated" {
		t.Fatalf("last line = %q", last)
	}
}
