package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

// TestExecutionSurface_Flags runs real-shaped diffs through ParseDiff and
// the gate, so quoting and renames are exercised as git writes them.
func TestExecutionSurface_Flags(t *testing.T) {
	const mod = "index 1111111..2222222 100644\n"
	cases := []struct {
		name, diff string
		flagged    bool
	}{
		{"package.json scripts", "diff --git a/package.json b/package.json\n" + mod +
			"--- a/package.json\n+++ b/package.json\n@@ -1 +1 @@\n-{\"scripts\": {}}\n+{\"scripts\": {\"postinstall\": \"sh x\"}}\n", true},
		{"husky hook added", "diff --git a/.husky/pre-commit b/.husky/pre-commit\nnew file mode 100644\nindex 0000000..2222222\n" +
			"--- /dev/null\n+++ b/.husky/pre-commit\n@@ -0,0 +1 @@\n+curl x | sh\n", true},
		{"gitattributes nested", "diff --git a/src/.gitattributes b/src/.gitattributes\nnew file mode 100644\nindex 0000000..2222222\n" +
			"--- /dev/null\n+++ b/src/.gitattributes\n@@ -0,0 +1 @@\n+*.java filter=x\n", true},
		{"claude settings", "diff --git a/.claude/settings.json b/.claude/settings.json\n" + mod +
			"--- a/.claude/settings.json\n+++ b/.claude/settings.json\n@@ -1 +1 @@\n-{}\n+{\"hooks\": {}}\n", true},
		{"claude nested", "diff --git a/web/app/.claude/settings.json b/web/app/.claude/settings.json\nnew file mode 100644\nindex 0000000..2222222\n" +
			"--- /dev/null\n+++ b/web/app/.claude/settings.json\n@@ -0,0 +1 @@\n+{}\n", true},
		{"claude case-folded", "diff --git a/.Claude/settings.json b/.Claude/settings.json\nnew file mode 100644\nindex 0000000..2222222\n" +
			"--- /dev/null\n+++ b/.Claude/settings.json\n@@ -0,0 +1 @@\n+{}\n", true},
		// ".cl‌aude" as git quotes it: U+200C is \342\200\214.
		{"claude HFS-ignorable", "diff --git \"a/.cl\\342\\200\\214aude/settings.json\" \"b/.cl\\342\\200\\214aude/settings.json\"\nnew file mode 100644\nindex 0000000..2222222\n" +
			"--- /dev/null\n+++ \"b/.cl\\342\\200\\214aude/settings.json\"\n@@ -0,0 +1 @@\n+{}\n", true},
		{"quoted path", "diff --git \"a/sub dir\\t/package.json\" \"b/sub dir\\t/package.json\"\n" + mod +
			"--- \"a/sub dir\\t/package.json\"\t\n+++ \"b/sub dir\\t/package.json\"\t\n@@ -1 +1 @@\n-{}\n+{\"scripts\": {}}\n", true},
		{"renamed in", "diff --git a/notes.md b/CLAUDE.md\nsimilarity index 100%\nrename from notes.md\nrename to CLAUDE.md\n", true},
		{"renamed out", "diff --git a/AGENTS.md b/notes.md\nsimilarity index 100%\nrename from AGENTS.md\nrename to notes.md\n", true},
		{"made executable", "diff --git a/tool.sh b/tool.sh\nold mode 100644\nnew mode 100755\n", true},
		{"new executable", "diff --git a/run b/run\nnew file mode 100755\nindex 0000000..2222222\n--- /dev/null\n+++ b/run\n@@ -0,0 +1 @@\n+#!/bin/sh\n", true},
		{"ordinary change", "diff --git a/src/App.java b/src/App.java\n" + mod +
			"--- a/src/App.java\n+++ b/src/App.java\n@@ -1 +1 @@\n-a\n+b\n", false},
		{"executable edited", "diff --git a/run b/run\nindex 1111111..2222222 100755\n--- a/run\n+++ b/run\n@@ -1 +1 @@\n-a\n+b\n", false},
		{"executable cleared", "diff --git a/tool.sh b/tool.sh\nold mode 100755\nnew mode 100644\n", false},
	}
	p := testPolicy(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkExecSurface(mustParse(t, []byte(tc.diff)), p)
			if v.Gate != contracts.GateExecSurface || !v.Passed || v.Flagged != tc.flagged {
				t.Fatalf("got %+v, want passed and flagged=%v", v, tc.flagged)
			}
		})
	}
}

func TestCheckExecSurface_RepoAddition(t *testing.T) {
	p := testPolicy(t)
	p.ExecutionSurfacePaths = []string{"scripts/"}
	change := []FileChange{{Kind: ChangeModified, OldPath: "scripts/release", NewPath: "scripts/release", Added: []string{"x"}}}
	v := checkExecSurface(change, p)
	if !v.Flagged || !strings.Contains(v.Detail, `"scripts/"`) {
		t.Fatalf("got %+v, want flagged by the repository's addition", v)
	}
}

func TestCheckExecSurface_Fixture(t *testing.T) {
	diff, err := os.ReadFile(filepath.Join("testdata", "flag_execution_surface.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if v := checkExecSurface(mustParse(t, diff), testPolicy(t)); !v.Passed || !v.Flagged {
		t.Fatalf("flag_execution_surface.diff not flagged: %+v", v)
	}
}
