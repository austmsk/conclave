package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker/dockertest"
)

// modifyA is a valid patch to src/a.go; numstatA is git's reading of it.
const (
	modifyA  = "diff --git a/src/a.go b/src/a.go\n--- a/src/a.go\n+++ b/src/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	numstatA = "1\t1\tsrc/a.go\x00"
)

// fakeGit answers the patch scripts through agenttest's ExecFunc. The
// first status call is the snapshot before the apply, the second the one
// after.
type fakeGit struct {
	check       contracts.ExecResult
	before      string
	after       string
	afterExit   int
	statusCalls int
}

func (g *fakeGit) exec(_ context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
	switch patchStep(req.Command) {
	case "check":
		return g.check, nil
	case "status":
		g.statusCalls++
		if g.statusCalls == 1 {
			return contracts.ExecResult{Stdout: []byte(g.before)}, nil
		}
		return contracts.ExecResult{Stdout: []byte(g.after), ExitCode: g.afterExit}, nil
	}
	return contracts.ExecResult{}, nil
}

// patchStep names which patch command a sandbox exec ran.
func patchStep(cmd []string) string {
	if len(cmd) > 0 && cmd[0] == "rm" {
		return "rm"
	}
	if len(cmd) < 3 {
		return "unknown"
	}
	switch cmd[2] {
	case patchCheckScript:
		return "check"
	case patchStatusScript:
		return "status"
	case patchApplyScript:
		return "apply"
	case patchRevertScript:
		return "revert"
	}
	return "unknown"
}

func patchSteps(calls []agenttest.Call) []string {
	var steps []string
	for _, c := range calls {
		steps = append(steps, patchStep(c.Command))
	}
	return steps
}

func runApplyPatch(ctx context.Context, sb contracts.Sandbox, patch string) (contracts.ToolResult, error) {
	args, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		return contracts.ToolResult{}, err
	}
	return newApplyPatch(toolEnv{maxCommandTimeout: time.Minute}).Execute(ctx, sb, args)
}

func wantPatchRefusal(t *testing.T, err error, code string, rejected bool) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("error = %v, want a *Refusal", err)
	}
	if r.Code != code || r.Rejected != rejected {
		t.Errorf("refusal = %q rejected=%v (%s), want %q rejected=%v", r.Code, r.Rejected, r.Msg, code, rejected)
	}
}

// patchRejectCase is one patch apply_patch must refuse.
type patchRejectCase struct {
	name  string
	patch string
	// git answers the scripts; nil means the patch must be refused
	// before anything runs in the sandbox.
	git *fakeGit
	// dockerSetup runs in a real sandbox first; dockerVerify must then
	// print "clean".
	dockerSetup, dockerVerify string
	wantCode                  string
	wantRejected              bool
}

func patchRejectCases() []patchRejectCase {
	return []patchRejectCase{
		{
			name:     "symlink mode in a traditional diff",
			patch:    "new file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/etc/passwd\n",
			wantCode: codePatchRejected, wantRejected: true,
		},
		{
			name:     "binary patch",
			patch:    "diff --git a/logo.png b/logo.png\nnew file mode 100644\nindex 0000000..1234567\nGIT binary patch\nliteral 3\nKcmZ?wbN~PV\n\n",
			wantCode: codePatchRejected, wantRejected: true,
		},
		{
			name:     "rename into .git",
			patch:    "diff --git a/hook.sh b/.git/hooks/pre-commit\nsimilarity index 100%\nrename from hook.sh\nrename to .git/hooks/pre-commit\n",
			wantCode: codeInvalidPath, wantRejected: true,
		},
		{
			name:     "quoted path",
			patch:    "diff --git \"a/caf\\303\\251.txt\" \"b/caf\\303\\251.txt\"\n--- \"a/caf\\303\\251.txt\"\n+++ \"b/caf\\303\\251.txt\"\n@@ -1 +1 @@\n-a\n+b\n",
			wantCode: codePatchRejected, wantRejected: true,
		},
		{
			name:     "gitlink",
			patch:    "diff --git a/lib b/lib\nindex 1111111..2222222 160000\n--- a/lib\n+++ b/lib\n@@ -1 +1 @@\n-Subproject commit 1111111\n+Subproject commit 2222222\n",
			wantCode: codePatchRejected, wantRejected: true,
		},
		{
			name:     "oversized patch",
			patch:    "--- a/big.txt\n+++ b/big.txt\n@@ -0,0 +1 @@\n+" + strings.Repeat("x", maxPatchBytes),
			wantCode: codePatchTooLarge, wantRejected: false,
		},
		{
			name:     "dot-dot path",
			patch:    "--- a/../outside.txt\n+++ b/../outside.txt\n@@ -1 +1 @@\n-a\n+b\n",
			wantCode: codeInvalidPath, wantRejected: true,
		},
		{
			name:     "absolute path",
			patch:    "--- /dev/null\n+++ /etc/cron.d/evil\n@@ -0,0 +1 @@\n+x\n",
			wantCode: codeInvalidPath, wantRejected: true,
		},
		{
			name:     "git sees a path the parser did not",
			patch:    modifyA,
			git:      &fakeGit{check: contracts.ExecResult{Stdout: []byte(numstatA + "1\t0\t.git/hooks/post-checkout\x00")}},
			wantCode: codePatchRejected, wantRejected: true,
		},
	}
}

// patchDockerRejectCases need a real sandbox: the symlink checks run in
// the container.
func patchDockerRejectCases() []patchRejectCase {
	return []patchRejectCase{
		{
			name:         "target under a symlinked directory the repo ships",
			patch:        "--- /dev/null\n+++ b/linkdir/escaped.txt\n@@ -0,0 +1 @@\n+escaped\n",
			dockerSetup:  patchShipSymlink,
			dockerVerify: "[ -e /tmp/escaped.txt ] && echo escaped || echo clean",
			wantCode:     codeInvalidPath, wantRejected: true,
		},
		{
			name:         "rewriting a symlink the repo ships",
			patch:        "--- a/linkdir\n+++ b/linkdir\n@@ -1 +1 @@\n-/tmp\n\\ No newline at end of file\n+/etc\n\\ No newline at end of file\n",
			dockerSetup:  patchShipSymlink,
			dockerVerify: `[ "$(readlink linkdir)" = /tmp ] && echo clean || echo changed`,
			wantCode:     codeInvalidPath, wantRejected: true,
		},
	}
}

func TestApplyPatchRejects(t *testing.T) {
	for _, tc := range append(patchRejectCases(), patchDockerRejectCases()...) {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dockerSetup != "" {
				h := dockertest.New(t)
				sb := h.Create(t, h.Spec(t, contracts.NetworkNone))
				h.Out(t, sb, tc.dockerSetup)
				_, err := runApplyPatch(h.Ctx, sb, tc.patch)
				wantPatchRefusal(t, err, tc.wantCode, tc.wantRejected)
				if got := h.Out(t, sb, tc.dockerVerify); got != "clean" {
					t.Errorf("after the refusal the sandbox shows %q", got)
				}
				return
			}
			sb := agenttest.NewSandbox(t)
			if tc.git != nil {
				sb.ExecFunc = tc.git.exec
			}
			_, err := runApplyPatch(context.Background(), sb, tc.patch)
			wantPatchRefusal(t, err, tc.wantCode, tc.wantRejected)
			wantNothingApplied(t, sb, tc.git == nil)
		})
	}
}

// wantNothingApplied checks a refused patch ran at most git's check, or
// nothing at all when it was refused in Go.
func wantNothingApplied(t *testing.T, sb *agenttest.Sandbox, refusedInGo bool) {
	t.Helper()
	steps := patchSteps(sb.Calls())
	if refusedInGo && len(steps) != 0 {
		t.Errorf("sandbox steps = %q, want none", steps)
	}
	for _, step := range steps {
		if step != "check" {
			t.Errorf("sandbox ran %q after the patch was refused", step)
		}
	}
}

// patchShipSymlink commits a symlink to the sandbox's /tmp, as a repository
// could ship one.
const patchShipSymlink = `ln -s /tmp linkdir && git -c user.name=t -c user.email=t@example.invalid add linkdir && ` +
	`git -c user.name=t -c user.email=t@example.invalid commit -q -m link && echo ok`

func TestApplyPatchGitSummaryMismatch(t *testing.T) {
	cases := []struct {
		name  string
		check string
	}{
		{"numstat lists an extra path", numstatA + "1\t0\tsrc/extra.go\x00"},
		{"numstat names a different path", "1\t1\tsrc/other.go\x00"},
		{"numstat lists fewer files than the headers", ""},
		{"binary in numstat", "-\t-\tsrc/a.go\x00"},
		{"unreadable numstat", "src/a.go\x00"},
		{"summary creates an unvalidated path", numstatA + " create mode 100644 src/extra.go\n"},
		{"summary names a symlink mode", numstatA + " mode change 100644 => 120000 src/a.go\n"},
		{"summary names a gitlink mode", numstatA + " create mode 160000 src/a.go\n"},
		{"summary rename the headers did not name", numstatA + " rename src/{a.go => b.go} (90%)\n"},
		{"summary line of an unknown form", numstatA + " frobnicate src/a.go\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			git := &fakeGit{check: contracts.ExecResult{Stdout: []byte(tc.check)}}
			sb.ExecFunc = git.exec
			_, err := runApplyPatch(context.Background(), sb, modifyA)
			wantPatchRefusal(t, err, codePatchRejected, true)
			if got := patchSteps(sb.Calls()); !reflect.DeepEqual(got, []string{"check"}) {
				t.Errorf("sandbox steps = %q, want only the check: the apply must never run", got)
			}
		})
	}
}

func TestApplyPatchSummaryAccepted(t *testing.T) {
	patch := modifyA +
		"diff --git a/src/old.go b/src/new.go\nold mode 100644\nnew mode 100755\nsimilarity index 100%\nrename from src/old.go\nrename to src/new.go\n" +
		"diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+hi\n"
	check := numstatA + "0\t0\tsrc/new.go\x001\t0\tn.txt\x00" +
		" rename src/{old.go => new.go} (100%)\n mode change 100644 => 100755\n create mode 100644 n.txt\n"
	sb := agenttest.NewSandbox(t)
	git := &fakeGit{
		check:  contracts.ExecResult{Stdout: []byte(check)},
		before: "?? notes.txt\x00",
		after:  "?? notes.txt\x00 M src/a.go\x00 D src/old.go\x00?? n.txt\x00?? src/new.go\x00",
	}
	sb.ExecFunc = git.exec
	res, err := runApplyPatch(context.Background(), sb, patch)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "Applied patch to 3 file(s)") {
		t.Errorf("output = %q", res.Output)
	}
	if got, want := patchSteps(sb.Calls()), []string{"check", "status", "apply", "status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q, want %q", got, want)
	}
	// The check walks every validated path; so does the status after.
	wantWalk := []string{"src/a.go", "src/old.go", "src/new.go", "n.txt"}
	if got := sb.Calls()[0].Command[5:]; !reflect.DeepEqual(got, wantWalk) {
		t.Errorf("check walked %q, want %q", got, wantWalk)
	}
	if got := sb.Calls()[3].Command[4:]; !reflect.DeepEqual(got, wantWalk) {
		t.Errorf("post-apply status walked %q, want %q", got, wantWalk)
	}
}

func TestApplyPatchRevertsUnexpectedChange(t *testing.T) {
	cases := []struct {
		name        string
		before      string
		after       string
		afterExit   int
		wantSteps   []string
		wantRevert  []string
		wantRemoved []string
	}{
		{
			name:        "an unnamed tracked file and an unnamed new file changed",
			before:      "?? notes.txt\x00",
			after:       "?? notes.txt\x00 M src/a.go\x00 M README.md\x00?? stray.txt\x00",
			wantSteps:   []string{"check", "status", "apply", "status", "revert", "rm"},
			wantRevert:  []string{"src/a.go", "README.md"},
			wantRemoved: []string{"stray.txt"},
		},
		{
			name:        "an unnamed untracked file disappeared",
			before:      "?? notes.txt\x00",
			after:       " M src/a.go\x00",
			wantSteps:   []string{"check", "status", "apply", "status", "revert"},
			wantRevert:  []string{"src/a.go"},
			wantRemoved: nil,
		},
		{
			name:        "a validated path became a symlink",
			after:       " T src/a.go\x00",
			afterExit:   exitPatchSymlink,
			wantSteps:   []string{"check", "status", "apply", "status", "revert"},
			wantRevert:  []string{"src/a.go"},
			wantRemoved: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			git := &fakeGit{check: contracts.ExecResult{Stdout: []byte(numstatA)}, before: tc.before, after: tc.after, afterExit: tc.afterExit}
			sb.ExecFunc = git.exec
			_, err := runApplyPatch(context.Background(), sb, modifyA)
			wantPatchRefusal(t, err, codePatchRejected, true)
			calls := sb.Calls()
			if got := patchSteps(calls); !reflect.DeepEqual(got, tc.wantSteps) {
				t.Fatalf("steps = %q, want %q", got, tc.wantSteps)
			}
			if got := calls[4].Command[4:]; !reflect.DeepEqual(got, tc.wantRevert) {
				t.Errorf("checkout restored %q, want %q", got, tc.wantRevert)
			}
			if tc.wantRemoved != nil {
				if got := calls[5].Command[3:]; !reflect.DeepEqual(got, tc.wantRemoved) {
					t.Errorf("rm removed %q, want %q", got, tc.wantRemoved)
				}
			}
		})
	}
}

func TestApplyPatchKeepsEarlierChanges(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	git := &fakeGit{
		check:  contracts.ExecResult{Stdout: []byte(numstatA)},
		before: " M README.md\x00?? notes.txt\x00",
		after:  " M README.md\x00 M src/a.go\x00?? notes.txt\x00",
	}
	sb.ExecFunc = git.exec
	if _, err := runApplyPatch(context.Background(), sb, modifyA); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := patchSteps(sb.Calls()), []string{"check", "status", "apply", "status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q, want %q", got, want)
	}
}

func TestApplyPatchGitFailureIsAResult(t *testing.T) {
	cases := []struct {
		name      string
		git       *fakeGit
		wantSteps []string
	}{
		{
			name:      "check fails",
			git:       &fakeGit{check: contracts.ExecResult{ExitCode: 1, Stderr: []byte("error: patch failed: src/a.go:1\n")}},
			wantSteps: []string{"check"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			sb.ExecFunc = tc.git.exec
			res, err := runApplyPatch(context.Background(), sb, modifyA)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.ExitCode != 1 || !strings.Contains(res.Output, "patch failed") || !strings.Contains(res.Output, "nothing was applied") {
				t.Errorf("result = %+v", res)
			}
			if got := patchSteps(sb.Calls()); !reflect.DeepEqual(got, tc.wantSteps) {
				t.Errorf("steps = %q, want %q", got, tc.wantSteps)
			}
		})
	}
}

func TestApplyPatchScriptRefusals(t *testing.T) {
	cases := []struct {
		name         string
		exit         int
		wantCode     string
		wantRejected bool
	}{
		{"symlink on a path", exitPatchSymlink, codeInvalidPath, true},
		{"path is a directory", exitPatchNotFile, codePatchNotFile, false},
		{"patch could not be staged", exitPatchStaging, codePatchStaging, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			sb.ExecFunc = (&fakeGit{check: contracts.ExecResult{ExitCode: tc.exit}}).exec
			_, err := runApplyPatch(context.Background(), sb, modifyA)
			wantPatchRefusal(t, err, tc.wantCode, tc.wantRejected)
		})
	}
}

func TestApplyPatchArguments(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"not an object", `"patch"`},
		{"unknown field", `{"patch":"x","path":"y"}`},
		{"trailing data", `{"patch":"x"} {}`},
		{"missing patch", `{}`},
		{"empty patch", `{"patch":""}`},
		{"null patch", `{"patch":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			_, err := newApplyPatch(toolEnv{}).Execute(context.Background(), sb, json.RawMessage(tc.args))
			wantPatchRefusal(t, err, codePatchArgs, false)
			if len(sb.Calls()) != 0 {
				t.Errorf("sandbox calls = %v, want none", sb.Calls())
			}
		})
	}
}

func TestApplyPatchSandboxErrorIsFatal(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	_, err := runApplyPatch(context.Background(), sb, modifyA)
	if !errors.Is(err, agenttest.ErrNoExec) {
		t.Fatalf("error = %v, want the sandbox's error wrapped", err)
	}
	if out := classify(err); !out.fatal {
		t.Errorf("classify = %+v, want fatal", out)
	}
}

func TestPatchFitsInOneArgument(t *testing.T) {
	// Linux's MAX_ARG_STRLEN: one argv string may be at most 32 pages.
	const maxArgStrlen = 32 * 4096
	if n := (maxPatchBytes + 2) / 3 * 4; n >= maxArgStrlen {
		t.Errorf("a %d-byte patch encodes to %d bytes, not under %d", maxPatchBytes, n, maxArgStrlen)
	}
}
