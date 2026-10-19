package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/docker/dockertest"
)

// The tests in this file run the tools against the real Docker sandbox,
// whose shell, find and grep are BusyBox's: the security claims are about
// that image, not the host's tools.

const outsideSecret = "canary_outside_secret"

// outsideSetup plants a directory outside the workspace and three ways to
// reach it from inside: a symlinked directory, a symlinked file, and a
// relative symlink one level down.
const outsideSetup = `mkdir -p /tmp/outside && echo ` + outsideSecret + ` > /tmp/outside/secret.txt &&
ln -s /tmp/outside link && ln -s /tmp/outside/secret.txt leak.txt &&
mkdir -p real && ln -s ../../tmp/outside real/up && echo ok`

func dockerToolSandbox(t *testing.T) (*dockertest.Harness, contracts.Sandbox) {
	t.Helper()
	h := dockertest.New(t)
	sb := h.Create(t, h.Spec(t, contracts.NetworkNone))
	return h, sb
}

// assertOutsideUntouched checks nothing was written outside the
// workspace.
func assertOutsideUntouched(t *testing.T, h *dockertest.Harness, sb contracts.Sandbox) {
	t.Helper()
	if got := h.Out(t, sb, "ls -A /tmp/outside; cat /tmp/outside/secret.txt"); got != "secret.txt\n"+outsideSecret {
		t.Errorf("a write escaped the workspace: /tmp/outside now holds %q", got)
	}
}

// TestAttackToolsRefuseSymlinks proves every path tool refuses a path
// whose component is a symlink out of the workspace, as a rejection the
// model sees, and that nothing outside changes.
//
// The apply_patch case is written against the tool's eventual behaviour
// and fails until apply_patch (built separately) lands.
func TestAttackToolsRefuseSymlinks(t *testing.T) {
	h, sb := dockerToolSandbox(t)
	h.Out(t, sb, outsideSetup)
	env := fileToolEnv()

	patchKey := patchArgName(newApplyPatch(env))
	cases := []struct {
		tool contracts.Tool
		args []map[string]any
	}{
		{newReadFile(env), pathArgs("link/secret.txt", "leak.txt", "real/up/secret.txt")},
		{newWriteFile(env), withArg(pathArgs("link/pwned.txt", "leak.txt", "real/up/pwned.txt"), "content", "pwned")},
		{newListDir(env), pathArgs("link", "real/up")},
		{newSearch(env), withArg(pathArgs("link", "leak.txt", "real/up", "real/up/secret.txt"), "pattern", "canary")},
		{newApplyPatch(env), []map[string]any{
			{patchKey: "diff --git a/link/pwned.txt b/link/pwned.txt\nnew file mode 100644\n--- /dev/null\n+++ b/link/pwned.txt\n@@ -0,0 +1 @@\n+pwned\n"},
			{patchKey: "--- a/leak.txt\n+++ b/leak.txt\n@@ -1 +1 @@\n-" + outsideSecret + "\n+pwned\n"},
		}},
	}
	for _, tc := range cases {
		t.Run(string(tc.tool.Name()), func(t *testing.T) {
			for _, args := range tc.args {
				res, err := tc.tool.Execute(h.Ctx, sb, toolArgs(t, args))
				if out := classify(err); err == nil || !out.rejected || out.fatal {
					t.Errorf("%s(%v) = %q, %v; want a rejection", tc.tool.Name(), args, res.Output, err)
				}
				if strings.Contains(res.Output, outsideSecret) {
					t.Errorf("%s(%v) returned content from outside the workspace", tc.tool.Name(), args)
				}
			}
			assertOutsideUntouched(t, h, sb)
		})
	}
}

// TestSearchDoesNotFollowSymlinkOut proves a search over a tree that
// holds symlinks out of the workspace never reads through them: not a
// symlinked file (which BusyBox grep -r would open), not a symlinked
// directory, and not a hardlink to something in .git.
func TestSearchDoesNotFollowSymlinkOut(t *testing.T) {
	h, sb := dockerToolSandbox(t)
	h.Out(t, sb, outsideSetup+` && mkdir -p src && echo needle_ok > src/ok.txt &&
ln -s /tmp/outside/secret.txt src/leak.txt && ln -s /tmp/outside src/outdir && ln -s ../../tmp/outside src/up &&
echo canary_git_description > .git/description && ln .git/description src/notes.txt && echo ok`)
	tool := newSearch(fileToolEnv())
	for _, p := range []string{"", "src"} {
		res, err := tool.Execute(h.Ctx, sb, toolArgs(t, map[string]any{"pattern": "canary_|needle_ok", "path": p}))
		if err != nil {
			t.Fatalf("search in %q: %v", p, err)
		}
		if res.Output != "src/ok.txt:1:needle_ok\n" {
			t.Errorf("search in %q = %q, want only src/ok.txt", p, res.Output)
		}
	}
	for _, p := range []string{"src/outdir", "src/up", "src/leak.txt", "link"} {
		_, err := tool.Execute(h.Ctx, sb, toolArgs(t, map[string]any{"pattern": "canary", "path": p}))
		if !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("search in %q = %v, want ErrInvalidPath", p, err)
		}
	}
}

// TestFileToolsInDocker covers ordinary behaviour against BusyBox, whose
// output the parsers must read.
func TestFileToolsInDocker(t *testing.T) {
	h, sb := dockerToolSandbox(t)
	env := fileToolEnv("secrets")
	h.Out(t, sb, `mkdir -p secrets && echo needle_secret > secrets/key.pem && echo needle_dash > ./-f && echo ok`)

	if _, err := newWriteFile(env).Execute(h.Ctx, sb, toolArgs(t, map[string]any{"path": "pkg/new.go", "content": "a\nneedle_new\nc\n"})); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	res, err := newReadFile(env).Execute(h.Ctx, sb, toolArgs(t, map[string]any{"path": "pkg/new.go", "start_line": 2, "end_line": 2}))
	if err != nil || res.Output != "needle_new\n" {
		t.Fatalf("read_file = %q, %v", res.Output, err)
	}
	res, err = newListDir(env).Execute(h.Ctx, sb, toolArgs(t, map[string]any{}))
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	for _, want := range []string{"-f\n", "pkg/\n"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("list_dir root = %q, missing %q", res.Output, want)
		}
	}
	for _, hidden := range []string{".git/", "secrets/"} {
		if strings.Contains("\n"+res.Output, "\n"+hidden+"\n") {
			t.Errorf("list_dir root = %q, shows %s", res.Output, hidden)
		}
	}
	res, err = newSearch(env).Execute(h.Ctx, sb, toolArgs(t, map[string]any{"pattern": "needle_"}))
	if err != nil || sorted(res.Output) != "-f:1:needle_dash\npkg/new.go:2:needle_new" {
		t.Fatalf("search = %q, %v", res.Output, err)
	}
	res, err = newSearch(env).Execute(h.Ctx, sb, toolArgs(t, map[string]any{"pattern": "needle", "path": "-f"}))
	if err != nil || res.Output != "-f:1:needle_dash\n" {
		t.Fatalf("search in -f = %q, %v", res.Output, err)
	}
	if _, err := newSearch(env).Execute(h.Ctx, sb, toolArgs(t, map[string]any{"pattern": "(unclosed"})); !isRefusal(err, codeBadPattern) {
		t.Fatalf("bad pattern = %v, want %s", err, codeBadPattern)
	}
}

func isRefusal(err error, code string) bool {
	var r *Refusal
	return errors.As(err, &r) && r.Code == code
}

func pathArgs(paths ...string) []map[string]any {
	out := make([]map[string]any, len(paths))
	for i, p := range paths {
		out[i] = map[string]any{"path": p}
	}
	return out
}

func withArg(args []map[string]any, key, value string) []map[string]any {
	for _, a := range args {
		a[key] = value
	}
	return args
}

// patchArgName reads apply_patch's argument name from its schema, so this
// test does not fix a name the tool's owner chooses.
func patchArgName(tool contracts.Tool) string {
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil || len(schema.Required) == 0 {
		return "patch"
	}
	return schema.Required[0]
}
