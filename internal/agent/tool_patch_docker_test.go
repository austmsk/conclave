package agent

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/docker/dockertest"
)

// fixturePatch edits the dockertest fixture repository: one modification,
// one new file, one rename with a mode change, one deletion.
const fixturePatch = `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1,3 @@
 # Election Tally
+
+Counts ballots.
diff --git a/docs/usage.md b/docs/usage.md
new file mode 100644
--- /dev/null
+++ b/docs/usage.md
@@ -0,0 +1 @@
+Run the tally.
diff --git a/src/tally.go b/src/count.go
old mode 100644
new mode 100755
similarity index 100%
rename from src/tally.go
rename to src/count.go
diff --git a/.gitignore b/.gitignore
deleted file mode 100644
--- a/.gitignore
+++ /dev/null
@@ -1 +0,0 @@
-node_modules/
`

func TestApplyPatchAppliesDocker(t *testing.T) {
	h := dockertest.New(t)
	sb := h.Create(t, h.Spec(t, contracts.NetworkNone))
	// An earlier, unrelated change must not trip the post-apply check.
	if err := sb.WriteFile(h.Ctx, "notes.txt", []byte("earlier edit\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res, err := runApplyPatch(h.Ctx, sb, fixturePatch)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "Applied patch to 4 file(s)") {
		t.Fatalf("result = %+v", res)
	}

	files := []struct{ path, want string }{
		{"README.md", "# Election Tally\n\nCounts ballots.\n"},
		{"docs/usage.md", "Run the tally.\n"},
		{"src/count.go", "package tally\n\nfunc Count() int { return 0 }\n"},
		{"notes.txt", "earlier edit\n"},
	}
	for _, f := range files {
		got, err := sb.ReadFile(h.Ctx, f.path)
		if err != nil || string(got) != f.want {
			t.Errorf("%s = %q, %v; want %q", f.path, got, err, f.want)
		}
	}
	for _, gone := range []string{"src/tally.go", ".gitignore"} {
		if _, err := sb.ReadFile(h.Ctx, gone); !errors.Is(err, sandbox.ErrNotFile) {
			t.Errorf("%s still readable: %v", gone, err)
		}
	}
	if got := h.Out(t, sb, "[ -x src/count.go ] && echo executable"); got != "executable" {
		t.Errorf("src/count.go mode change not applied: %q", got)
	}
	if got := h.Out(t, sb, "ls -A /tmp | wc -l"); got != "0" {
		t.Errorf("the staged patch was left in /tmp: %s", h.Out(t, sb, "ls -A /tmp"))
	}
}

// TestApplyPatchLargestPatchDocker sends a patch at the size cap through
// a real exec, where it travels base64-encoded in one argv string.
func TestApplyPatchLargestPatchDocker(t *testing.T) {
	h := dockertest.New(t)
	sb := h.Create(t, h.Spec(t, contracts.NetworkNone))
	line := "+" + strings.Repeat("x", 99) + "\n"
	const header = "--- /dev/null\n+++ b/big.txt\n@@ -0,0 +1,%d @@\n"
	n := (maxPatchBytes - len(header) - 8) / len(line)
	patch := fmt.Sprintf(header, n) + strings.Repeat(line, n)
	if len(patch) > maxPatchBytes || len(patch) < maxPatchBytes-len(line)-16 {
		t.Fatalf("test patch is %d bytes; want just under %d", len(patch), maxPatchBytes)
	}

	res, err := runApplyPatch(h.Ctx, sb, patch)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if got := h.Out(t, sb, "wc -l < big.txt"); got != strconv.Itoa(n) {
		t.Errorf("big.txt has %s lines, want %d", got, n)
	}
}

func TestApplyPatchContextMismatchDocker(t *testing.T) {
	h := dockertest.New(t)
	sb := h.Create(t, h.Spec(t, contracts.NetworkNone))
	patch := "--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# Some Other Project\n+# Renamed\n"

	res, err := runApplyPatch(h.Ctx, sb, patch)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Output, "patch does not apply") {
		t.Errorf("result = %+v, want git's failure as the output", res)
	}
	if got, err := sb.ReadFile(h.Ctx, "README.md"); err != nil || string(got) != "# Election Tally\n" {
		t.Errorf("README.md = %q, %v; want it unchanged", got, err)
	}
}
