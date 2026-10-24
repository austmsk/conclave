package gate

import (
	"os"
	"path/filepath"
	"testing"
)

// w1NewFileDiff is a new one-line file at the path as git writes it in a
// diff --git header, quoted or not.
func w1NewFileDiff(header, mode string) string {
	return "diff --git a/" + header + " b/" + header + "\nnew file mode " + mode + "\nindex 0000000..2222222\n" +
		"--- /dev/null\n+++ b/" + header + "\n@@ -0,0 +1 @@\n+x\n"
}

// w1QuotedNewFileDiff is w1NewFileDiff for a path git quotes; quoted is
// the C-quoted path without its a/ or b/ prefix or quotes.
func w1QuotedNewFileDiff(quoted string) string {
	return "diff --git \"a/" + quoted + "\" \"b/" + quoted + "\"\nnew file mode 100644\nindex 0000000..2222222\n" +
		"--- /dev/null\n+++ \"b/" + quoted + "\"\n@@ -0,0 +1 @@\n+x\n"
}

// TestAttackArtifactPaths feeds hostile paths, shaped as git would write
// them, through ParseDiff and the protected-paths gate. Each must fail
// the gate: none may reach staging or the owner's checkout.
func TestAttackArtifactPaths(t *testing.T) {
	cases := []struct{ name, diff string }{
		{"dot git upper case", w1NewFileDiff(".GIT/config", "100644")},
		{"dot git hooks", w1NewFileDiff("sub/.git/hooks/pre-commit", "100644")},
		{"NTFS short name", w1NewFileDiff("git~1/config", "100644")},
		{"NTFS alternate stream", w1NewFileDiff(".git::$INDEX_ALLOCATION/config", "100644")},
		{"NTFS trailing dot", w1NewFileDiff(".git./config", "100644")},
		{"HFS ignorable in dot git", w1QuotedNewFileDiff(".g\\342\\200\\214it/config")},
		{"parent traversal", w1NewFileDiff("../outside.txt", "100644")},
		{"inner traversal", w1NewFileDiff("src/../../outside.txt", "100644")},
		{"dots only", w1NewFileDiff("..../x", "100644")},
		{"absolute", w1NewFileDiff("/etc/passwd", "100644")},
		{"bidi override", w1QuotedNewFileDiff("src/evil\\342\\200\\256txt.java")},
		{"bidi isolate", w1QuotedNewFileDiff("\\342\\201\\246README.md")},
		{"control character", w1QuotedNewFileDiff("src/a\\033[31mb.txt")},
		{"newline in path", w1QuotedNewFileDiff("src/a\\nb.txt")},
		{"invalid UTF-8", w1QuotedNewFileDiff("src/\\377.txt")},
		{"symlink new file", w1NewFileDiff("docs/link", "120000")},
		{"symlink to outside", "diff --git a/l b/l\nnew file mode 120000\nindex 0000000..2222222\n--- /dev/null\n+++ b/l\n@@ -0,0 +1 @@\n+../../../../etc\n\\ No newline at end of file\n"},
		{"file replaced by symlink", "diff --git a/s b/s\nold mode 100644\nnew mode 120000\n"},
		{"gitlink", "diff --git a/vendor/lib b/vendor/lib\nnew file mode 160000\nindex 0000000..2222222\n--- /dev/null\n+++ b/vendor/lib\n@@ -0,0 +1 @@\n+Subproject commit 2222222222222222222222222222222222222222\n"},
		{"renamed into dot git", "diff --git a/hook.sh b/.git/hooks/post-checkout\nsimilarity index 100%\nrename from hook.sh\nrename to .git/hooks/post-checkout\n"},
		{"copied into dot git", "diff --git a/x b/.Git/info/attributes\nsimilarity index 100%\ncopy from x\ncopy to .Git/info/attributes\n"},
	}
	p := testPolicy(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes := mustParse(t, []byte(tc.diff))
			if v := checkProtected(changes, p); v.Passed {
				t.Fatalf("hostile path passed protected_paths: %+v", v)
			}
		})
	}
}

// TestStaticGates_Fixtures checks each testdata fixture fails or flags
// exactly the static gate it is named for, and pass_static passes all.
// fail_secrets is for the secrets gate, so every gate here passes it.
func TestStaticGates_Fixtures(t *testing.T) {
	type outcome struct{ size, protected, weakening, surfaceFlag bool }
	pass := outcome{size: true, protected: true, weakening: true}
	cases := map[string]outcome{
		"pass_static":            pass,
		"fail_secrets":           pass,
		"fail_diff_size":         {size: false, protected: true, weakening: true},
		"fail_protected_paths":   {size: true, protected: false, weakening: true},
		"fail_gate_weakening":    {size: true, protected: true, weakening: false},
		"flag_execution_surface": {size: true, protected: true, weakening: true, surfaceFlag: true},
	}
	p := testPolicy(t)
	for _, name := range []string{"pass_static", "fail_secrets", "fail_diff_size", "fail_protected_paths", "fail_gate_weakening", "flag_execution_surface"} {
		want := cases[name]
		t.Run(name, func(t *testing.T) {
			diff, err := os.ReadFile(filepath.Join("testdata", name+".diff"))
			if err != nil {
				t.Fatal(err)
			}
			changes := mustParse(t, diff)
			surface := checkExecSurface(changes, p)
			got := outcome{
				size:        checkDiffSize(changes, len(diff), p.Limits).Passed,
				protected:   checkProtected(changes, p).Passed,
				weakening:   checkWeakening(changes, p).Passed,
				surfaceFlag: surface.Flagged,
			}
			if got != want || !surface.Passed {
				t.Fatalf("got %+v (surface passed %v), want %+v", got, surface.Passed, want)
			}
		})
	}
}
