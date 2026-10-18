package agent

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// parseCase is a patch the parser accepts and what it should read.
type parseCase struct {
	name      string
	patch     string
	wantFiles []patchFile
	wantPaths []string
}

func runParseCases(t *testing.T, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePatch(tc.patch, nil)
			if err != nil {
				t.Fatalf("parsePatch: %v", err)
			}
			if !reflect.DeepEqual(got.files, tc.wantFiles) {
				t.Errorf("files = %+v, want %+v", got.files, tc.wantFiles)
			}
			if !reflect.DeepEqual(got.paths, tc.wantPaths) {
				t.Errorf("paths = %q, want %q", got.paths, tc.wantPaths)
			}
		})
	}
}

func TestParsePatchGitHeaders(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "git modification",
			patch: "diff --git a/src/a.go b/src/a.go\nindex 1111111..2222222 100644\n--- a/src/a.go\n+++ b/src/a.go\n" +
				"@@ -1 +1 @@\n-old\n+new\n",
			wantFiles: []patchFile{{oldPath: "src/a.go", newPath: "src/a.go"}},
			wantPaths: []string{"src/a.go"},
		},
		{
			name:      "git new file from /dev/null",
			patch:     "diff --git a/n.txt b/n.txt\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+hi\n",
			wantFiles: []patchFile{{newPath: "n.txt"}},
			wantPaths: []string{"n.txt"},
		},
		{
			name:      "git deletion to /dev/null",
			patch:     "diff --git a/d.txt b/d.txt\ndeleted file mode 100755\n--- a/d.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
			wantFiles: []patchFile{{oldPath: "d.txt"}},
			wantPaths: []string{"d.txt"},
		},
		{
			name:      "mode change takes its name from the diff --git line",
			patch:     "diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n",
			wantFiles: []patchFile{{oldPath: "run.sh", newPath: "run.sh"}},
			wantPaths: []string{"run.sh"},
		},
		{
			name:      "diff --git name with spaces",
			patch:     "diff --git a/my file.txt b/my file.txt\nold mode 100644\nnew mode 100755\n",
			wantFiles: []patchFile{{oldPath: "my file.txt", newPath: "my file.txt"}},
			wantPaths: []string{"my file.txt"},
		},
		{
			name:      "diff --git names are validated even when --- and +++ name another file",
			patch:     "diff --git a/a.go b/a.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x\n+y\n",
			wantFiles: []patchFile{{oldPath: "b.go", newPath: "b.go"}},
			wantPaths: []string{"a.go", "b.go"},
		},
		{
			name:      "a bare diff --git line is skipped as git skips it",
			patch:     "diff --git a/a.go b/a.go\n\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x\n+y\n",
			wantFiles: []patchFile{{oldPath: "b.go", newPath: "b.go"}},
			wantPaths: []string{"a.go", "b.go"},
		},
	})
}

func TestParsePatchRenameCopyHeaders(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name:      "rename from and to",
			patch:     "diff --git a/src/old.go b/src/new.go\nsimilarity index 90%\nrename from src/old.go\nrename to src/new.go\n",
			wantFiles: []patchFile{{oldPath: "src/old.go", newPath: "src/new.go", isRename: true}},
			wantPaths: []string{"src/old.go", "src/new.go"},
		},
		{
			name:      "rename old and new",
			patch:     "diff --git a/x b/y\nrename old x\nrename new y\n",
			wantFiles: []patchFile{{oldPath: "x", newPath: "y", isRename: true}},
			wantPaths: []string{"x", "y"},
		},
		{
			name:      "copy from and to",
			patch:     "diff --git a/src/a.go b/lib/b.go\nsimilarity index 100%\ncopy from src/a.go\ncopy to lib/b.go\n",
			wantFiles: []patchFile{{oldPath: "src/a.go", newPath: "lib/b.go", isCopy: true}},
			wantPaths: []string{"src/a.go", "lib/b.go"},
		},
		{
			name:      "rename with a mode change",
			patch:     "diff --git a/run b/bin/run\nold mode 100644\nnew mode 100755\nrename from run\nrename to bin/run\n",
			wantFiles: []patchFile{{oldPath: "run", newPath: "bin/run", isRename: true}},
			wantPaths: []string{"run", "bin/run"},
		},
	})
}

func TestParsePatchTraditionalHeaders(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name:      "traditional diff",
			patch:     "--- a/x.txt\n+++ b/x.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+three\n",
			wantFiles: []patchFile{{oldPath: "x.txt", newPath: "x.txt"}},
			wantPaths: []string{"x.txt"},
		},
		{
			name:      "traditional creation with timestamps after a tab",
			patch:     "--- /dev/null\t2026-01-01 00:00:00\n+++ b/new.txt\t2026-01-01 00:00:00\n@@ -0,0 +1 @@\n+x\n",
			wantFiles: []patchFile{{newPath: "new.txt"}},
			wantPaths: []string{"new.txt"},
		},
		{
			name:      "traditional diff with differing names validates both",
			patch:     "--- a/one.txt\n+++ b/two.txt\n@@ -1 +1 @@\n-a\n+b\n",
			wantFiles: []patchFile{{oldPath: "one.txt", newPath: "two.txt"}},
			wantPaths: []string{"one.txt", "two.txt"},
		},
		{
			name: "hunk body lines are not headers",
			patch: "--- a/notes.md\n+++ b/notes.md\n@@ -1,2 +1,2 @@\n--- a/.git/config\n+++ b/.git/config\n" +
				"-diff --git a/.git/x b/.git/x\n+GIT binary patch\n",
			wantFiles: []patchFile{{oldPath: "notes.md", newPath: "notes.md"}},
			wantPaths: []string{"notes.md"},
		},
		{
			name:      "no newline marker and empty context line",
			patch:     "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n\n-two\n\\ No newline at end of file\n+2\n\\ No newline at end of file\n",
			wantFiles: []patchFile{{oldPath: "a.txt", newPath: "a.txt"}},
			wantPaths: []string{"a.txt"},
		},
		{
			name: "commentary before and between sections",
			patch: "Fix the tally.\n\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n" +
				"-- \n2.49.1\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x\n+y\n",
			wantFiles: []patchFile{{oldPath: "a.go", newPath: "a.go"}, {oldPath: "b.go", newPath: "b.go"}},
			wantPaths: []string{"a.go", "b.go"},
		},
	})
}

func TestParsePatchRefuses(t *testing.T) {
	cases := []struct {
		name         string
		patch        string
		wantCode     string
		wantRejected bool
	}{
		{"quoted diff --git line", "diff --git \"a/caf\\303\\251\" \"b/caf\\303\\251\"\nold mode 100644\nnew mode 100755\n", codePatchRejected, true},
		{"quoted --- name", "--- \"a/x\\n\"\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n", codePatchRejected, true},
		{"quoted rename target", "diff --git a/x b/y\nrename from x\nrename to \"y\"\n", codePatchRejected, true},
		{"GIT binary patch", "diff --git a/x.png b/x.png\nnew file mode 100644\nindex 0000000..1111111\nGIT binary patch\nliteral 1\nIcmZ?w0001\n", codePatchRejected, true},
		{"binary files differ", "diff --git a/x.png b/x.png\nindex 1111111..2222222 100644\nBinary files a/x.png and b/x.png differ\n", codePatchRejected, true},
		{"symlink as a new file", "diff --git a/l b/l\nnew file mode 120000\n--- /dev/null\n+++ b/l\n@@ -0,0 +1 @@\n+/etc\n", codePatchRejected, true},
		{"symlink in an index line", "diff --git a/l b/l\nindex 1111111..2222222 120000\n--- a/l\n+++ b/l\n@@ -1 +1 @@\n-a\n+/etc\n", codePatchRejected, true},
		{"gitlink mode change", "diff --git a/lib b/lib\nold mode 100644\nnew mode 160000\n", codePatchRejected, true},
		{"symlink mode before a traditional diff", "new file mode 120000\n--- /dev/null\n+++ b/l\n@@ -0,0 +1 @@\n+/etc\n", codePatchRejected, true},
		{"odd mode", "diff --git a/x b/x\nold mode 100644\nnew mode 100777\n", codePatchRejected, true},
		{"dot-dot path", "--- a/../x\n+++ b/../x\n@@ -1 +1 @@\n-a\n+b\n", codeInvalidPath, true},
		{"absolute path", "--- /dev/null\n+++ /etc/cron.d/x\n@@ -0,0 +1 @@\n+x\n", codeInvalidPath, true},
		{"absolute rename target", "diff --git a/x b/y\nrename from x\nrename to /etc/passwd\n", codeInvalidPath, true},
		{"rename into .git", "diff --git a/h b/.git/hooks/pre-commit\nrename from h\nrename to .git/hooks/pre-commit\n", codeInvalidPath, true},
		{".git in a header-only section", "diff --git a/.git/config b/.git/config\nold mode 100644\nnew mode 100755\n", codeInvalidPath, true},
		{"missing a/ prefix", "--- x.txt\n+++ x.txt\n@@ -1 +1 @@\n-a\n+b\n", codePatchInvalid, false},
		{"both sides /dev/null", "--- /dev/null\n+++ /dev/null\n@@ -0,0 +0,0 @@\n", codePatchInvalid, false},
		{"malformed hunk header", "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1\n-a\n+b\n", codePatchInvalid, false},
		{"hunk shorter than its header", "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n-a\n+b\n", codePatchInvalid, false},
		{"hunk longer than its header", "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n-b\n+c\n", codePatchInvalid, false},
		{"hunk without a header", "@@ -1 +1 @@\n-a\n+b\n", codePatchInvalid, false},
		{"no file sections", "just some text\n", codePatchInvalid, false},
		{"inconsistent names", "diff --git a/x b/y\nrename from x\nrename to y\n--- a/z\n+++ b/y\n@@ -1 +1 @@\n-a\n+b\n", codePatchInvalid, false},
		{"header without a name", "diff --git a/x b/y\nold mode 100644\nnew mode 100755\n", codePatchInvalid, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePatch(tc.patch, nil)
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("parsePatch error = %v, want a *Refusal", err)
			}
			if r.Code != tc.wantCode || r.Rejected != tc.wantRejected {
				t.Errorf("refusal = %q rejected=%v (%s), want %q rejected=%v", r.Code, r.Rejected, r.Msg, tc.wantCode, tc.wantRejected)
			}
			if tc.name != "no file sections" && !strings.HasPrefix(r.Msg, "patch line ") {
				t.Errorf("message %q does not say which line", r.Msg)
			}
		})
	}
}

func TestGitDefaultName(t *testing.T) {
	cases := []struct{ rest, want string }{
		{"a/x b/x", "x"},
		{"a/dir/x y b/dir/x y", "dir/x y"},
		{"a/x\tb/x", "x"},
		{"a/x b/y", ""},
		{"x x", ""},
		{"/x /x", ""},
	}
	for _, tc := range cases {
		if got := gitDefaultName(tc.rest); got != tc.want {
			t.Errorf("gitDefaultName(%q) = %q, want %q", tc.rest, got, tc.want)
		}
	}
}

func TestRenameSummary(t *testing.T) {
	cases := []struct{ old, new, want string }{
		{"src/old.go", "src/new.go", "src/{old.go => new.go}"},
		{"src/old.go", "lib/copy.go", "src/old.go => lib/copy.go"},
		{"a/b/c.go", "a/b/d/c.go", "a/b/{c.go => d/c.go}"},
		{"x", "y", "x => y"},
	}
	for _, tc := range cases {
		if got := renameSummary(tc.old, tc.new); got != tc.want {
			t.Errorf("renameSummary(%q, %q) = %q, want %q", tc.old, tc.new, got, tc.want)
		}
	}
}
