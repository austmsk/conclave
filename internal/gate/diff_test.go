package gate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// w1GitDiff builds a repository in a temporary directory, commits what
// the base script writes, runs the change script, and returns the diff
// exactly as the sandbox's Diff produces it. Both scripts are sh run in
// the repository with git's configuration isolated.
func w1GitDiff(t testing.TB, base, change string) []byte {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	home := t.TempDir()
	script := strings.Join([]string{
		"git init -q -b main .",
		"git config user.email gate@example.invalid",
		"git config user.name gate",
		base,
		"git add -A",
		"git commit -q --allow-empty -m base",
		"base=$(git rev-parse HEAD)",
		change,
		"git add -A -N . 2>/dev/null",
		`exec git diff --binary --no-color --no-ext-diff "$base"`,
	}, "\n")
	cmd := exec.Command("sh", "-e", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + home,
		"PATH=" + os.Getenv("PATH"), "LC_ALL=C",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git script: %v\n%s", err, stderr.String())
	}
	return out
}

// mustParse parses a diff that must be well formed.
func mustParse(t testing.TB, diff []byte) []FileChange {
	t.Helper()
	changes, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff: %v\n%s", err, diff)
	}
	return changes
}

// w1Fixtures are the recipes for testdata/*.diff. Set
// GATE_UPDATE_FIXTURES=1 to regenerate them with the local git.
var w1Fixtures = []struct{ name, base, change string }{
	{
		name:   "fail_diff_size",
		base:   "mkdir src && printf 'class App {}\\n' > src/App.java",
		change: "i=0; while [ $i -lt 450 ]; do echo \"    int f$i = $i;\"; i=$((i+1)); done > src/Big.java",
	},
	{
		name:   "fail_protected_paths",
		base:   "mkdir -p .github/workflows src && printf 'on: push\\njobs: {}\\n' > .github/workflows/ci.yml && printf 'class App {}\\n' > src/App.java",
		change: "printf 'on: push\\njobs:\\n  leak: {}\\n' > .github/workflows/ci.yml",
	},
	{
		name:   "fail_gate_weakening",
		base:   "mkdir src && printf 'class App {}\\n' > src/App.java && printf 'class FooTest {\\n    @Test\\n    void adds() {}\\n}\\n' > src/FooTest.java",
		change: "rm src/FooTest.java",
	},
	{
		name:   "flag_execution_surface",
		base:   "printf '{\\n  \"name\": \"app\",\\n  \"scripts\": {}\\n}\\n' > package.json",
		change: "printf '{\\n  \"name\": \"app\",\\n  \"scripts\": {\"postinstall\": \"node x.js\"}\\n}\\n' > package.json",
	},
	{
		name:   "fail_secrets",
		base:   "mkdir src && printf 'class App {}\\n' > src/App.java",
		change: "printf 'class Config {\\n    static final String TOKEN = \"canary_gate_7f3a9c\";\\n}\\n' > src/Config.java",
	},
	{
		name:   "pass_static",
		base:   "mkdir src && printf 'class App {\\n}\\n' > src/App.java && printf 'class AppTest {\\n    @Test\\n    void runs() {}\\n}\\n' > src/AppTest.java",
		change: "printf 'class App {\\n    int answer() { return 42; }\\n}\\n' > src/App.java && printf 'class AppTest {\\n    @Test\\n    void runs() {}\\n\\n    @Test\\n    void answers() {}\\n}\\n' > src/AppTest.java",
	},
}

func TestDiffFixtures(t *testing.T) {
	update := os.Getenv("GATE_UPDATE_FIXTURES") != ""
	for _, f := range w1Fixtures {
		t.Run(f.name, func(t *testing.T) {
			file := filepath.Join("testdata", f.name+".diff")
			if update {
				if err := os.WriteFile(file, w1GitDiff(t, f.base, f.change), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			diff, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if len(mustParse(t, diff)) == 0 {
				t.Fatal("fixture has no changes")
			}
		})
	}
}

func TestParseDiff_RealGit(t *testing.T) {
	for _, tc := range realGitCases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustParse(t, w1GitDiff(t, tc.base, tc.change))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

type parseCase struct {
	name, base, change string
	want               []FileChange
}

// realGitCases are changes made in a real repository and the
// FileChanges they must parse to.
var realGitCases = []parseCase{
	{
		name: "modified", base: "printf 'a\\nb\\n' > f.txt", change: "printf 'a\\nc\\n' > f.txt",
		want: []FileChange{{Kind: ChangeModified, OldPath: "f.txt", NewPath: "f.txt", OldMode: "100644", NewMode: "100644", Added: []string{"c"}, Removed: []string{"b"}, AddedAt: []int{2}, RemovedAt: []int{2}}},
	},
	{
		name: "added", change: "printf 'x\\ny\\n' > new.txt",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "new.txt", NewMode: "100644", Added: []string{"x", "y"}, AddedAt: []int{1, 2}}},
	},
	{
		name: "added empty", change: ": > empty.txt",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "empty.txt", NewMode: "100644"}},
	},
	{
		name: "deleted", base: "printf 'gone\\n' > d.txt", change: "rm d.txt",
		want: []FileChange{{Kind: ChangeDeleted, OldPath: "d.txt", OldMode: "100644", Removed: []string{"gone"}, RemovedAt: []int{1}}},
	},
	{
		name: "renamed unchanged", base: "printf '1\\n2\\n3\\n4\\n' > old.txt", change: "mv old.txt new.txt",
		want: []FileChange{{Kind: ChangeRenamed, OldPath: "old.txt", NewPath: "new.txt"}},
	},
	{
		name: "renamed and edited", base: "printf '1\\n2\\n3\\n4\\n5\\n6\\n' > old.txt", change: "mv old.txt new.txt && printf '1\\n2\\n3\\n4\\n5\\nsix\\n' > new.txt",
		want: []FileChange{{Kind: ChangeRenamed, OldPath: "old.txt", NewPath: "new.txt", OldMode: "100644", NewMode: "100644", Added: []string{"six"}, Removed: []string{"6"}, AddedAt: []int{6}, RemovedAt: []int{6}}},
	},
	{
		name: "copied", base: "git config diff.renames copies && printf '1\\n2\\n3\\n4\\n5\\n6\\n' > a.txt", change: "cp a.txt b.txt && printf '1\\n2\\n3\\n4\\n5\\n7\\n' > a.txt",
		want: []FileChange{
			{Kind: ChangeModified, OldPath: "a.txt", NewPath: "a.txt", OldMode: "100644", NewMode: "100644", Added: []string{"7"}, Removed: []string{"6"}, AddedAt: []int{6}, RemovedAt: []int{6}},
			{Kind: ChangeCopied, OldPath: "a.txt", NewPath: "b.txt"},
		},
	},
	{
		name: "made executable", base: "printf '#!/bin/sh\\n' > s.sh", change: "chmod +x s.sh",
		want: []FileChange{{Kind: ChangeModified, OldPath: "s.sh", NewPath: "s.sh", OldMode: "100644", NewMode: "100755"}},
	},
	{
		name: "new executable", change: "printf '#!/bin/sh\\n' > run && chmod +x run",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "run", NewMode: "100755", Added: []string{"#!/bin/sh"}, AddedAt: []int{1}}},
	},
	{
		name: "binary modified", base: "printf 'bin\\000ary' > b.bin", change: "printf 'bin\\000ary2' > b.bin",
		want: []FileChange{{Kind: ChangeModified, OldPath: "b.bin", NewPath: "b.bin", OldMode: "100644", NewMode: "100644", Binary: true}},
	},
	{
		name: "binary added", change: "printf 'new\\000bin' > n.bin",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "n.bin", NewMode: "100644", Binary: true}},
	},
	{
		name: "binary large", change: "i=0; while [ $i -lt 200 ]; do printf 'chunk %d \\000\\001\\002' $i; i=$((i+1)); done > big.bin",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "big.bin", NewMode: "100644", Binary: true}},
	},
	{
		name: "no newline at end", base: "printf 'x' > n.txt", change: "printf 'y' > n.txt",
		want: []FileChange{{Kind: ChangeModified, OldPath: "n.txt", NewPath: "n.txt", OldMode: "100644", NewMode: "100644", Added: []string{"y"}, Removed: []string{"x"}, AddedAt: []int{1}, RemovedAt: []int{1}}},
	},
	{
		name: "newline added at end", base: "printf 'x' > n.txt", change: "printf 'x\\n' > n.txt",
		want: []FileChange{{Kind: ChangeModified, OldPath: "n.txt", NewPath: "n.txt", OldMode: "100644", NewMode: "100644", Added: []string{"x"}, Removed: []string{"x"}, AddedAt: []int{1}, RemovedAt: []int{1}}},
	},
	{
		name: "symlink retargeted", base: "ln -s a.txt link", change: "rm link && ln -s b.txt link",
		want: []FileChange{{Kind: ChangeModified, OldPath: "link", NewPath: "link", OldMode: "120000", NewMode: "120000", Added: []string{"b.txt"}, Removed: []string{"a.txt"}, AddedAt: []int{1}, RemovedAt: []int{1}}},
	},
	{
		name: "symlink added", change: "ln -s ../../etc/passwd link",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "link", NewMode: "120000", Added: []string{"../../etc/passwd"}, AddedAt: []int{1}}},
	},
	{
		name: "path with space", base: "printf 'k\\n' > 'sp ace.txt'", change: "printf 'n\\n' > 'sp ace.txt'",
		want: []FileChange{{Kind: ChangeModified, OldPath: "sp ace.txt", NewPath: "sp ace.txt", OldMode: "100644", NewMode: "100644", Added: []string{"n"}, Removed: []string{"k"}, AddedAt: []int{1}, RemovedAt: []int{1}}},
	},
	{
		name: "new path with space", change: "printf 'n\\n' > 'new sp.txt'",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "new sp.txt", NewMode: "100644", Added: []string{"n"}, AddedAt: []int{1}}},
	},
	{
		name: "path containing b/", change: "mkdir 'x b' && printf 'n\\n' > 'x b/y'",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "x b/y", NewMode: "100644", Added: []string{"n"}, AddedAt: []int{1}}},
	},
	{
		name: "quoted path", change: "printf 'q\\n' > \"$(printf 'tab\\t\\303\\251 \"q\".txt')\"",
		want: []FileChange{{Kind: ChangeAdded, NewPath: "tab\té \"q\".txt", NewMode: "100644", Added: []string{"q"}, AddedAt: []int{1}}},
	},
	{
		name: "quoted rename", base: "printf '1\\n2\\n3\\n' > plain.txt", change: "mv plain.txt \"$(printf 'na\\tme.txt')\"",
		want: []FileChange{{Kind: ChangeRenamed, OldPath: "plain.txt", NewPath: "na\tme.txt"}},
	},
	{
		name: "rename with spaces", base: "printf '1\\n2\\n3\\n' > 'a b.txt'", change: "mv 'a b.txt' 'c b.txt'",
		want: []FileChange{{Kind: ChangeRenamed, OldPath: "a b.txt", NewPath: "c b.txt"}},
	},
	{
		name: "crlf content", base: "printf 'a\\r\\nb\\r\\n' > w.txt", change: "printf 'a\\r\\nc\\r\\n' > w.txt",
		want: []FileChange{{Kind: ChangeModified, OldPath: "w.txt", NewPath: "w.txt", OldMode: "100644", NewMode: "100644", Added: []string{"c\r"}, Removed: []string{"b\r"}, AddedAt: []int{2}, RemovedAt: []int{2}}},
	},
	{
		name: "two hunks", base: "i=1; while [ $i -le 30 ]; do echo $i; i=$((i+1)); done > h.txt", change: "sed -e 's/^2$/two/' -e 's/^29$/twenty-nine/' h.txt > h2 && mv h2 h.txt",
		want: []FileChange{{Kind: ChangeModified, OldPath: "h.txt", NewPath: "h.txt", OldMode: "100644", NewMode: "100644", Added: []string{"two", "twenty-nine"}, Removed: []string{"2", "29"}, AddedAt: []int{2, 29}, RemovedAt: []int{2, 29}}},
	},
}

func TestParseDiff_Gitlink(t *testing.T) {
	diff := w1GitDiff(t, "", "mkdir sub && (cd sub && git init -q && git commit -q --allow-empty -m x)")
	got := mustParse(t, diff)
	if len(got) != 1 || got[0].Kind != ChangeAdded || got[0].NewPath != "sub" || got[0].NewMode != modeGitlink {
		t.Fatalf("got %#v, want one added gitlink sub", got)
	}
}

func TestParseDiff_Empty(t *testing.T) {
	got, err := ParseDiff(nil)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want no changes", got, err)
	}
}

// Hand-written shapes git emits that the real-git cases do not reach.
func TestParseDiff_Shapes(t *testing.T) {
	cases := []struct {
		name string
		diff string
		want []FileChange
	}{
		{
			name: "binary files differ",
			diff: "diff --git a/x.bin b/x.bin\nindex 1111111..2222222 100644\nBinary files a/x.bin and b/x.bin differ\n",
			want: []FileChange{{Kind: ChangeModified, OldPath: "x.bin", NewPath: "x.bin", OldMode: "100644", NewMode: "100644", Binary: true}},
		},
		{
			name: "binary delta sections",
			diff: "diff --git a/x.bin b/x.bin\nindex 1111111..2222222 100644\nGIT binary patch\ndelta 8\nPcmYew%wtF_sx$%s42c4`\n\ndelta 7\nOcmYew%wtF_sssQD(E^45\n\n",
			want: []FileChange{{Kind: ChangeModified, OldPath: "x.bin", NewPath: "x.bin", OldMode: "100644", NewMode: "100644", Binary: true}},
		},
		{
			name: "deleted empty file",
			diff: "diff --git a/e b/e\ndeleted file mode 100644\nindex e69de29..0000000\n",
			want: []FileChange{{Kind: ChangeDeleted, OldPath: "e", OldMode: "100644"}},
		},
		{
			name: "rename with mode change",
			diff: "diff --git a/a.sh b/b.sh\nold mode 100644\nnew mode 100755\nsimilarity index 100%\nrename from a.sh\nrename to b.sh\n",
			want: []FileChange{{Kind: ChangeRenamed, OldPath: "a.sh", NewPath: "b.sh", OldMode: "100644", NewMode: "100755"}},
		},
		{
			name: "hunk section heading",
			diff: "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@ func main() {\n-a\n+b\n",
			want: []FileChange{{Kind: ChangeModified, OldPath: "f", NewPath: "f", OldMode: "100644", NewMode: "100644", Added: []string{"b"}, Removed: []string{"a"}, AddedAt: []int{1}, RemovedAt: []int{1}}},
		},
		{
			name: "no newline mid hunk",
			diff: "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1,2 @@\n-a\n\\ No newline at end of file\n+a\n+b\n",
			want: []FileChange{{Kind: ChangeModified, OldPath: "f", NewPath: "f", OldMode: "100644", NewMode: "100644", Added: []string{"a", "b"}, Removed: []string{"a"}, AddedAt: []int{1, 2}, RemovedAt: []int{1}}},
		},
		{
			name: "octal escapes",
			diff: "diff --git \"a/\\303\\251\" \"b/\\303\\251\"\nnew file mode 100644\nindex 0000000..e69de29\n",
			want: []FileChange{{Kind: ChangeAdded, NewPath: "é", NewMode: "100644"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustParse(t, []byte(tc.diff))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

const w1ModifiedEntry = "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"

// malformedDiffs must each be refused with ErrMalformedDiff.
var malformedDiffs = []struct{ name, diff string }{
	{"no trailing newline", strings.TrimSuffix(w1ModifiedEntry, "\n")},
	{"not a diff", "hello\n"},
	{"leading garbage", "junk\n" + w1ModifiedEntry},
	{"trailing garbage", w1ModifiedEntry + "junk\n"},
	{"unknown header", "diff --git a/f b/f\nfrobnicate 1\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"hunk too short", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n-a\n+b\n"},
	{"hunk too long", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n+c\n"},
	{"hunk extra context", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n c\n"},
	{"new side starts at line 0", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +0,1 @@\n-a\n+b\n"},
	{"old side starts at line 0", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -0,1 +1 @@\n-a\n+b\n"},
	{"hunks out of order", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -10 +10 @@\n-a\n+b\n@@ -1 +1 @@\n-c\n+d\n"},
	{"hunks overlap", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n-a\n+b\n x\n@@ -2 +2 @@\n-c\n+d\n"},
	{"hunk huge count", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1,999999999 +1 @@\n-a\n+b\n"},
	{"empty hunk", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1,0 +1,0 @@\n"},
	{"empty line in hunk", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n-a\n+b\n\n"},
	{"hunk missing", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n"},
	{"plus line missing", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"misplaced no newline", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n\\ No newline at end of file\n-a\n+b\n"},
	{"double no newline", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n\\ No newline at end of file\n\\ No newline at end of file\n+b\n"},
	{"other backslash line", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n\\ something else\n+b\n"},
	{"bad mode", "diff --git a/f b/f\nold mode 100644\nnew mode 100777\n"},
	{"same mode", "diff --git a/f b/f\nold mode 100644\nnew mode 100644\n"},
	{"old mode alone", "diff --git a/f b/f\nold mode 100644\n"},
	{"repeated header", "diff --git a/f b/f\nold mode 100644\nold mode 100644\nnew mode 100755\n"},
	{"new and deleted", "diff --git a/f b/f\nnew file mode 100644\ndeleted file mode 100644\n"},
	{"mode change on new file", "diff --git a/f b/f\nnew file mode 100644\nold mode 100644\nnew mode 100755\n"},
	{"no change", "diff --git a/f b/f\nindex 1111111..2222222 100644\n"},
	{"bare header", "diff --git a/f b/f\n"},
	{"patch path disagrees with header", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/g\n+++ b/g\n@@ -1 +1 @@\n-a\n+b\n"},
	{"minus and plus differ", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/g\n@@ -1 +1 @@\n-a\n+b\n"},
	{"new file not from dev null", "diff --git a/f b/f\nnew file mode 100644\nindex 0000000..2222222\n--- a/f\n+++ b/f\n@@ -0,0 +1 @@\n+b\n"},
	{"deleted file not to dev null", "diff --git a/f b/f\ndeleted file mode 100644\nindex 1111111..0000000\n--- a/f\n+++ b/f\n@@ -1 +0,0 @@\n-a\n"},
	{"modified to dev null", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-a\n"},
	{"rename without similarity", "diff --git a/a b/b\nrename from a\nrename to b\n"},
	{"rename half", "diff --git a/a b/b\nsimilarity index 100%\nrename from a\n"},
	{"rename and copy", "diff --git a/a b/b\nsimilarity index 100%\nrename from a\nrename to b\ncopy from a\ncopy to b\n"},
	{"rename disagrees with patch", "diff --git a/a b/b\nsimilarity index 90%\nrename from a\nrename to b\nindex 1111111..2222222 100644\n--- a/a\n+++ b/c\n@@ -1 +1 @@\n-a\n+b\n"},
	{"similarity without rename", "diff --git a/f b/f\nsimilarity index 90%\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"bad similarity", "diff --git a/a b/b\nsimilarity index 101%\nrename from a\nrename to b\n"},
	{"dissimilarity index", "diff --git a/f b/f\ndissimilarity index 90%\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"bad index", "diff --git a/f b/f\nindex xyz..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"index mode beside mode change", "diff --git a/f b/f\nold mode 100644\nnew mode 100755\nindex 1111111..2222222 100755\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"no prefix", "diff --git f f\nindex 1111111..2222222 100644\n--- f\n+++ f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"mnemonic prefix", "diff --git c/f w/f\nindex 1111111..2222222 100644\n--- c/f\n+++ w/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"rename header without prefix", "diff --git x/a y/b\nsimilarity index 100%\nrename from a\nrename to b\n"},
	{"unreadable header", "diff --git a/f b/g\nindex 1111111..2222222 100644\nBinary files a/f and b/g differ\n"},
	{"unterminated quote", "diff --git \"a/f \"b/f\"\nnew file mode 100644\nindex 0000000..e69de29\n"},
	{"invalid escape", "diff --git \"a/\\x41\" \"b/\\x41\"\nnew file mode 100644\nindex 0000000..e69de29\n"},
	{"octal out of range", "diff --git \"a/\\477\" \"b/\\477\"\nnew file mode 100644\nindex 0000000..e69de29\n"},
	{"raw control in quotes", "diff --git \"a/x\ty\" \"b/x\ty\"\nnew file mode 100644\nindex 0000000..e69de29\n"},
	{"unquoted high byte", "diff --git a/\xc3\xa9 b/\xc3\xa9\nnew file mode 100644\nindex 0000000..e69de29\n"},
	{"unquoted quote", "diff --git a/x\"y b/x\"y\nnew file mode 100644\nindex 0000000..e69de29\n"},
	{"missing space tab", "diff --git a/s p b/s p\nindex 1111111..2222222 100644\n--- a/s p\n+++ b/s p\n@@ -1 +1 @@\n-a\n+b\n"},
	{"stray tab", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\t\n+++ b/f\t\n@@ -1 +1 @@\n-a\n+b\n"},
	{"text after quoted path", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- \"a/f\"x\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"},
	{"binary bad length char", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\n0cmYew%wtF_sx$%s42c4`\n\n"},
	{"binary short line", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\nPcmYew%wtF_sx$%s42c4\n\n"},
	{"binary bad alphabet", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\nPcmYew%wtF_sx$%s42c4\"\n\n"},
	{"binary unterminated", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\nPcmYew%wtF_sx$%s42c4`\n"},
	{"binary empty section", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\n\n"},
	{"binary no size", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nPcmYew%wtF_sx$%s42c4`\n\n"},
	{"binary third section", "diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\nPcmYew%wtF_sx$%s42c4`\n\nliteral 8\nPcmYew%wtF_sx$%s42c4`\n\nliteral 8\nPcmYew%wtF_sx$%s42c4`\n\n"},
	{"line too long", "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+" + strings.Repeat("x", maxDiffLineBytes) + "\n"},
}

func TestParseDiff_Malformed(t *testing.T) {
	for _, tc := range malformedDiffs {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDiff([]byte(tc.diff))
			if !errors.Is(err, ErrMalformedDiff) {
				t.Fatalf("got %#v, %v; want ErrMalformedDiff", got, err)
			}
		})
	}
}

func TestParseDiff_Bounds(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= maxDiffFiles; i++ {
		fmt.Fprintf(&b, "diff --git a/f%d b/f%d\nold mode 100644\nnew mode 100755\n", i, i)
	}
	if _, err := ParseDiff([]byte(b.String())); !errors.Is(err, ErrMalformedDiff) {
		t.Fatalf("%d files: got %v, want ErrMalformedDiff", maxDiffFiles+1, err)
	}
	big := make([]byte, MaxArtifactBytes+1)
	if _, err := ParseDiff(big); !errors.Is(err, ErrMalformedDiff) {
		t.Fatalf("oversized: got %v, want ErrMalformedDiff", err)
	}
}

// TestParseDiff_ErrorsOmitContent checks a refusal never echoes the
// diff's content, which is the agent's.
func TestParseDiff_ErrorsOmitContent(t *testing.T) {
	diff := "diff --git a/f b/f\nindex 1111111..2222222 100644\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+canary_gate_err1\n+canary_gate_err2\n"
	_, err := ParseDiff([]byte(diff))
	if err == nil || strings.Contains(err.Error(), "canary_") {
		t.Fatalf("got %v; want an error without the content", err)
	}
}

func FuzzParseDiff(f *testing.F) {
	seeds, _ := filepath.Glob(filepath.Join("testdata", "*.diff"))
	for _, s := range seeds {
		if b, err := os.ReadFile(s); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte(w1ModifiedEntry))
	f.Add([]byte("diff --git \"a/\\303\\251 x\" \"b/\\303\\251 x\"\nnew file mode 100644\nindex 0000000..e69de29\n"))
	f.Add([]byte("diff --git a/a b/b\nsimilarity index 100%\nrename from a\nrename to b\n"))
	f.Add([]byte("diff --git a/x b/x\nindex 1111111..2222222 100644\nGIT binary patch\nliteral 8\nPcmYew%wtF_sx$%s42c4`\n\n"))
	f.Fuzz(func(t *testing.T, diff []byte) {
		changes, err := ParseDiff(diff)
		if err != nil {
			if !errors.Is(err, ErrMalformedDiff) {
				t.Fatalf("error does not wrap ErrMalformedDiff: %v", err)
			}
			return
		}
		for _, c := range changes {
			w1CheckChangeShape(t, c)
		}
	})
}

// w1CheckChangeShape checks the invariants every parsed change keeps.
func w1CheckChangeShape(t *testing.T, c FileChange) {
	t.Helper()
	// The secrets gate and the comment-boundary check rely on every
	// changed line having a line number, from 1 and strictly increasing.
	w1CheckLineNumbers(t, "added", len(c.Added), c.AddedAt)
	w1CheckLineNumbers(t, "removed", len(c.Removed), c.RemovedAt)
	switch c.Kind {
	case ChangeAdded:
		if c.OldPath != "" || c.NewPath == "" {
			t.Fatalf("added change with paths %q -> %q", c.OldPath, c.NewPath)
		}
	case ChangeDeleted:
		if c.OldPath == "" || c.NewPath != "" {
			t.Fatalf("deleted change with paths %q -> %q", c.OldPath, c.NewPath)
		}
	case ChangeModified, ChangeRenamed, ChangeCopied:
		if c.OldPath == "" || c.NewPath == "" {
			t.Fatalf("%s change with paths %q -> %q", c.Kind, c.OldPath, c.NewPath)
		}
	default:
		t.Fatalf("unknown kind %q", c.Kind)
	}
}

// w1CheckLineNumbers checks that at holds one line number per line, each
// at least 1 and greater than the one before.
func w1CheckLineNumbers(t *testing.T, side string, lines int, at []int) {
	t.Helper()
	if len(at) != lines {
		t.Fatalf("%d %s lines with %d line numbers", lines, side, len(at))
	}
	for i, n := range at {
		if n < 1 || (i > 0 && n <= at[i-1]) {
			t.Fatalf("%s line numbers %v are not from 1 and strictly increasing", side, at)
		}
	}
}
