package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/sandbox"
)

func TestListDirLists(t *testing.T) {
	cases := []struct{ name, path, want string }{
		{"root omitted", "", ".github/\nREADME.md\nempty/\nlink@\nsrc/\n"},
		{"root dot", ".", ".github/\nREADME.md\nempty/\nlink@\nsrc/\n"},
		{"subdirectory", "src", "-f/\na b.go\nmain.go\nnested/\n"},
		{"nested", "src/nested", "x.go\n"},
		{"empty", "empty", ""},
		{"leading dash", "src/-f", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			execOnHost(sb)
			seed(t, sb.Dir, map[string]string{
				"README.md": "r", ".github/workflows/ci.yml": "c", "src/main.go": "m",
				"src/a b.go": "s", "src/nested/x.go": "x",
			})
			mkdirs(t, sb.Dir, "empty", "src/-f")
			if err := os.Symlink("README.md", filepath.Join(sb.Dir, "link")); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{}
			if tc.path != "" {
				args["path"] = tc.path
			}
			res, err := newListDir(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, args))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Output != tc.want {
				t.Fatalf("output = %q, want %q", res.Output, tc.want)
			}
		})
	}
}

func mkdirs(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(n)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListDirFiltersDenylist(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	execOnHost(sb)
	seed(t, sb.Dir, map[string]string{
		".git/config": "c", "git~1/HEAD": "h", ".gitignore": "g",
		"secrets/key.pem": "canary_key", "secretsfoo/ok.txt": "ok",
		"config/prod.yaml": "canary_prod", "config/dev.yaml": "d",
		"src/main.go": "m",
	})
	env := fileToolEnv("secrets", "config/prod.yaml")
	cases := []struct{ path, want string }{
		{"", ".gitignore\nconfig/\nsecretsfoo/\nsrc/\n"},
		{"config", "dev.yaml\n"},
	}
	for _, tc := range cases {
		res, err := newListDir(env).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": tc.path}))
		if err != nil {
			t.Fatalf("list_dir(%q): %v", tc.path, err)
		}
		if res.Output != tc.want {
			t.Errorf("list_dir(%q) = %q, want %q", tc.path, res.Output, tc.want)
		}
	}
	for _, p := range []string{"secrets", "Secrets", ".git", "git~1"} {
		calls := len(sb.Calls())
		_, err := newListDir(env).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": p}))
		if !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("list_dir(%q) = %v, want ErrInvalidPath", p, err)
		}
		if len(sb.Calls()) != calls {
			t.Errorf("list_dir(%q) reached the sandbox", p)
		}
	}
}

func TestListDirRefusals(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	execOnHost(sb)
	seed(t, sb.Dir, map[string]string{"file.txt": "x"})
	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"missing", map[string]any{"path": "nope"}, codeNotFound},
		{"not a directory", map[string]any{"path": "file.txt"}, codeNotDirectory},
		{"unknown field", map[string]any{"path": "", "recursive": true}, codeBadArgs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newListDir(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, tc.args))
			var r *Refusal
			if !errors.As(err, &r) || r.Code != tc.code {
				t.Fatalf("Execute = %v, want refusal %s", err, tc.code)
			}
		})
	}
}

// TestFormatListingFailsClosed feeds the parser records a script could
// not have produced, or names the tools refuse, and checks none is shown.
func TestFormatListingFailsClosed(t *testing.T) {
	out := "fok.txt\x00" + // kept
		"dsrc\x00" + // kept
		"lln\x00" + // kept
		"x\x00" + // too short
		"qunknown\x00" + // unknown type
		"fa/b\x00" + // a slash cannot be in a name
		"d.git\x00" + "d.GIT\x00" + "dgit~1\x00" + // .git spellings
		"fbad\nname\x00" + "f\U0000202eevil\x00" + // control and bidi characters
		"dsecrets\x00" + // denylisted
		"fcut" // no terminator: a record cut short is dropped
	want := "ln@\nok.txt\nsrc/\n"
	if got := formatListing("", []byte(out), []string{"secrets"}); got != want {
		t.Fatalf("formatListing = %q, want %q", got, want)
	}
}
