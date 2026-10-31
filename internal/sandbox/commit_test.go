package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

const (
	botName  = "conclave[bot]"
	botEmail = "1234+conclave[bot]@users.noreply.github.com"
)

var commitWhen = time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("CET", 3600))

// commitFixture is a source repository with a base commit and a change
// committed on it, the change's binary diff, and prepared copies of the
// source taken at the base.
type commitFixture struct {
	src, base, head string
	diff            []byte
}

func newCommitFixture(t *testing.T) *commitFixture {
	t.Helper()
	src := t.TempDir()
	sandboxtest.Git(t, src, "init", "-q", "-b", "main", ".")
	sandboxtest.WriteFile(t, src, "README.md", "# Tally\n")
	sandboxtest.WriteFile(t, src, "src/tally.go", "package tally\n\nfunc Count() int { return 0 }\n")
	sandboxtest.Git(t, src, "add", "-A")
	sandboxtest.Git(t, src, "commit", "-q", "-m", "base")
	f := &commitFixture{src: src, base: strings.TrimSpace(sandboxtest.Git(t, src, "rev-parse", "HEAD"))}
	sandboxtest.WriteFile(t, src, "src/tally.go", "package tally\n\nfunc Count() int { return 1 }\n")
	sandboxtest.WriteFile(t, src, "data.bin", "\x00\x01\x02\xffbinary")
	if err := os.Remove(filepath.Join(src, "README.md")); err != nil {
		t.Fatal(err)
	}
	sandboxtest.Git(t, src, "add", "-A")
	sandboxtest.Git(t, src, "commit", "-q", "-m", "change")
	f.head = strings.TrimSpace(sandboxtest.Git(t, src, "rev-parse", "HEAD"))
	f.diff = []byte(sandboxtest.Git(t, src, "diff", "--binary", "--no-color", f.base, f.head))
	return f
}

// tree returns a prepared copy of the source checked out at the base.
func (f *commitFixture) tree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tree")
	if err := os.CopyFS(dir, os.DirFS(f.src)); err != nil {
		t.Fatal(err)
	}
	sandboxtest.Git(t, dir, "checkout", "-q", "--detach", f.base)
	if err := sandbox.PrepareTree(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *commitFixture) spec(dir string) sandbox.CommitSpec {
	return sandbox.CommitSpec{
		Dir: dir, Parent: f.base, Diff: f.diff,
		Message:    "feat(tally): count one\n\nTask: T1\nGenerated-by: conclave\n",
		AuthorName: botName, AuthorEmail: botEmail, When: commitWhen,
	}
}

func rev(t *testing.T, dir, what string) string {
	t.Helper()
	return strings.TrimSpace(sandboxtest.Git(t, dir, "rev-parse", what))
}

// TestCommitDiffDeterministic builds the same spec in two clones: the
// commit IDs match, and the commit's tree is exactly the change's tree,
// binary file and deletion included.
func TestCommitDiffDeterministic(t *testing.T) {
	f := newCommitFixture(t)
	var ids []string
	for range 2 {
		dir := f.tree(t)
		id, err := sandbox.CommitDiff(t.Context(), f.spec(dir))
		if err != nil {
			t.Fatalf("CommitDiff: %v", err)
		}
		if got, want := rev(t, dir, id+"^{tree}"), rev(t, f.src, f.head+"^{tree}"); got != want {
			t.Errorf("commit tree %s, want the change's tree %s", got, want)
		}
		ids = append(ids, id)
	}
	if ids[0] != ids[1] {
		t.Fatalf("same spec gave %s and %s", ids[0], ids[1])
	}
}

// TestCommitDiffRecordsIdentityAndDate reads the commit back: one parent,
// the bot as author and committer, the fixed date in UTC, and the message
// exactly as given.
func TestCommitDiffRecordsIdentityAndDate(t *testing.T) {
	f := newCommitFixture(t)
	dir := f.tree(t)
	spec := f.spec(dir)
	id, err := sandbox.CommitDiff(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	raw := sandboxtest.Git(t, dir, "cat-file", "-p", id)
	header, message, ok := strings.Cut(raw, "\n\n")
	if !ok {
		t.Fatalf("commit object %q has no message", raw)
	}
	// 12:00 CET is 11:00 UTC; the date is recorded in UTC.
	stamp := "conclave[bot] <" + botEmail + "> " + strconv.FormatInt(time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC).Unix(), 10) + " +0000"
	for _, want := range []string{"parent " + f.base, "author " + stamp, "committer " + stamp} {
		if !strings.Contains(header, want+"\n") && !strings.HasSuffix(header, want) {
			t.Errorf("commit header lacks %q:\n%s", want, header)
		}
	}
	if strings.Count(header, "parent ") != 1 {
		t.Errorf("commit has %d parents, want 1", strings.Count(header, "parent "))
	}
	if message != spec.Message {
		t.Errorf("message = %q, want %q", message, spec.Message)
	}
}

// TestCommitDiffLeavesTreeAlone: the work tree, HEAD and the real index
// are untouched, and the tree still passes VerifyTree, as Push requires.
func TestCommitDiffLeavesTreeAlone(t *testing.T) {
	f := newCommitFixture(t)
	dir := f.tree(t)
	if _, err := sandbox.CommitDiff(t.Context(), f.spec(dir)); err != nil {
		t.Fatal(err)
	}
	if got := rev(t, dir, "HEAD"); got != f.base {
		t.Errorf("HEAD = %s, want %s", got, f.base)
	}
	if st := sandboxtest.Git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("work tree or index changed:\n%s", st)
	}
	if err := sandbox.VerifyTree(dir); err != nil {
		t.Errorf("VerifyTree after commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data.bin")); !os.IsNotExist(err) {
		t.Errorf("the diff's file was checked out: %v", err)
	}
}

func TestCommitDiffEmptyDiffKeepsParentTree(t *testing.T) {
	f := newCommitFixture(t)
	dir := f.tree(t)
	spec := f.spec(dir)
	spec.Diff = nil
	id, err := sandbox.CommitDiff(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rev(t, dir, id+"^{tree}"), rev(t, dir, f.base+"^{tree}"); got != want {
		t.Errorf("tree %s, want the parent's %s", got, want)
	}
}

func TestCommitDiffRefusesDiffThatDoesNotApply(t *testing.T) {
	f := newCommitFixture(t)
	dir := f.tree(t)
	spec := f.spec(dir)
	spec.Diff = []byte("diff --git a/src/tally.go b/src/tally.go\n--- a/src/tally.go\n+++ b/src/tally.go\n" +
		"@@ -1,3 +1,3 @@\n package tally\n \n-func Missing() {}\n+func Other() {}\n")
	_, err := sandbox.CommitDiff(t.Context(), spec)
	if !errors.Is(err, sandbox.ErrCommitApply) {
		t.Fatalf("got %v, want ErrCommitApply", err)
	}
	if strings.Contains(err.Error(), "Missing") {
		t.Errorf("error quotes the diff: %v", err)
	}
}

func TestCommitDiffRefusesBadSpec(t *testing.T) {
	f := newCommitFixture(t)
	dir := f.tree(t)
	unprepared := filepath.Join(t.TempDir(), "raw")
	if err := os.CopyFS(unprepared, os.DirFS(f.src)); err != nil {
		t.Fatal(err)
	}
	sandboxtest.Git(t, unprepared, "remote", "add", "origin", sandboxtest.RemoteURL)
	cases := map[string]func(*sandbox.CommitSpec){
		"relative tree":      func(s *sandbox.CommitSpec) { s.Dir = "tree" },
		"unprepared tree":    func(s *sandbox.CommitSpec) { s.Dir = unprepared },
		"short parent":       func(s *sandbox.CommitSpec) { s.Parent = f.base[:12] },
		"parent as a ref":    func(s *sandbox.CommitSpec) { s.Parent = "HEAD" },
		"empty message":      func(s *sandbox.CommitSpec) { s.Message = " \n" },
		"no date":            func(s *sandbox.CommitSpec) { s.When = time.Time{} },
		"no author":          func(s *sandbox.CommitSpec) { s.AuthorName = "" },
		"newline in author":  func(s *sandbox.CommitSpec) { s.AuthorName = "bot\ncommitter evil <e@x> 0 +0000" },
		"bracket in email":   func(s *sandbox.CommitSpec) { s.AuthorEmail = "a>b@example.invalid" },
		"angle in author":    func(s *sandbox.CommitSpec) { s.AuthorName = "bot <x@y>" },
		"NUL in the email":   func(s *sandbox.CommitSpec) { s.AuthorEmail = "a\x00@example.invalid" },
		"whitespace author":  func(s *sandbox.CommitSpec) { s.AuthorName = "  " },
		"whitespace email":   func(s *sandbox.CommitSpec) { s.AuthorEmail = "\t" },
		"carriage in author": func(s *sandbox.CommitSpec) { s.AuthorName = "bot\r" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := f.spec(dir)
			mutate(&spec)
			if _, err := sandbox.CommitDiff(t.Context(), spec); !errors.Is(err, sandbox.ErrCommitSpec) {
				t.Fatalf("got %v, want ErrCommitSpec", err)
			}
		})
	}
}
