package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

func TestPrepareTreeStripsEveryTraceOfTheRemote(t *testing.T) {
	dir, head := sandboxtest.NewRepo(t)
	if hits := sandboxtest.ContainsToken(t, filepath.Join(dir, ".git")); len(hits) == 0 {
		t.Fatal("fixture should plant the canary token before preparation")
	}

	if err := sandbox.PrepareTree(dir); err != nil {
		t.Fatalf("PrepareTree: %v", err)
	}

	if hits := sandboxtest.ContainsToken(t, dir); len(hits) != 0 {
		t.Errorf("canary token survived in %v", hits)
	}
	if out := sandboxtest.Git(t, dir, "remote"); strings.TrimSpace(out) != "" {
		t.Errorf("remotes remain: %q", out)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"helper", "extraheader", "[remote", "github.com"} {
		if strings.Contains(strings.ToLower(string(cfg)), fragment) {
			t.Errorf("config still mentions %q:\n%s", fragment, cfg)
		}
	}
	if out := sandboxtest.Git(t, dir, "for-each-ref", "refs/remotes"); strings.TrimSpace(out) != "" {
		t.Errorf("remote-tracking refs remain: %q", out)
	}
	if got := strings.TrimSpace(sandboxtest.Git(t, dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("HEAD moved: got %s want %s", got, head)
	}
	if out := sandboxtest.Git(t, dir, "status", "--porcelain"); out != "" {
		t.Errorf("working tree dirty after preparation: %q", out)
	}
}

func TestPrepareTreeIsIdempotent(t *testing.T) {
	dir, _ := sandboxtest.NewRepo(t)
	for i := 0; i < 2; i++ {
		if err := sandbox.PrepareTree(dir); err != nil {
			t.Fatalf("PrepareTree call %d: %v", i+1, err)
		}
	}
	if err := sandbox.VerifyTree(dir); err != nil {
		t.Fatal(err)
	}
}

// unpreparedTrees are the shapes VerifyTree must refuse: each mutation
// reintroduces one thing PrepareTree strips.
var unpreparedTrees = []struct {
	name   string
	mutate func(t *testing.T, dir string)
}{
	{"fresh clone with remote", func(t *testing.T, dir string) {}},
	{"remote section only", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.Git(t, dir, "remote", "add", "origin", sandboxtest.RemoteURL)
	}},
	{"credential helper", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.Git(t, dir, "config", "credential.helper", "cache")
	}},
	{"extra header", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.Git(t, dir, "config", "http.extraheader", "Authorization: x")
	}},
	{"include", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.Git(t, dir, "config", "include.path", "/somewhere/else")
	}},
	{"ssh command", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.Git(t, dir, "config", "core.sshCommand", "ssh -i key")
	}},
	{"reflog", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.WriteFile(t, dir, ".git/logs/HEAD", "x")
	}},
	{"FETCH_HEAD", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.WriteFile(t, dir, ".git/FETCH_HEAD", sandboxtest.RemoteURL)
	}},
	{"packed remote ref", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.WriteFile(t, dir, ".git/packed-refs", "# pack-refs with: peeled\n"+strings.Repeat("a", 40)+" refs/remotes/origin/main\n")
	}},
	{"alternates", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		sandboxtest.WriteFile(t, dir, ".git/objects/info/alternates", "/elsewhere/objects")
	}},
	{"git file (worktree)", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
		sandboxtest.WriteFile(t, dir, ".git", "gitdir: /elsewhere/.git/worktrees/x")
	}},
	{"git symlink", func(t *testing.T, dir string) {
		mustPrepare(t, dir)
		real := filepath.Join(dir, ".git")
		moved := filepath.Join(t.TempDir(), "git")
		if err := os.Rename(real, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, real); err != nil {
			t.Fatal(err)
		}
	}},
}

func TestVerifyTreeRejectsUnpreparedTrees(t *testing.T) {
	for _, tc := range unpreparedTrees {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := sandboxtest.NewRepo(t)
			tc.mutate(t, dir)
			err := sandbox.VerifyTree(dir)
			if !errors.Is(err, sandbox.ErrTreeNotPrepared) {
				t.Fatalf("VerifyTree = %v, want ErrTreeNotPrepared", err)
			}
		})
	}
}

func TestPrepareTreeRefusesTreesItCannotMakeSafe(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{"alternates", func(t *testing.T, dir string) {
			sandboxtest.WriteFile(t, dir, ".git/objects/info/alternates", "/elsewhere/objects")
		}},
		{"missing git dir", func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := sandboxtest.NewRepo(t)
			tc.mutate(t, dir)
			if err := sandbox.PrepareTree(dir); !errors.Is(err, sandbox.ErrTreeNotPrepared) {
				t.Fatalf("PrepareTree = %v, want ErrTreeNotPrepared", err)
			}
		})
	}
}

func TestHeadCommit(t *testing.T) {
	dir, head := sandboxtest.NewRepo(t)
	mustPrepare(t, dir)

	t.Run("packed ref", func(t *testing.T) {
		got, err := sandbox.HeadCommit(dir)
		if err != nil || got != head {
			t.Fatalf("HeadCommit = %q, %v; want %q", got, err, head)
		}
	})
	t.Run("loose ref", func(t *testing.T) {
		sandboxtest.WriteFile(t, dir, "new.txt", "x")
		sandboxtest.Git(t, dir, "add", "new.txt")
		sandboxtest.Git(t, dir, "commit", "-q", "-m", "second")
		want := strings.TrimSpace(sandboxtest.Git(t, dir, "rev-parse", "HEAD"))
		got, err := sandbox.HeadCommit(dir)
		if err != nil || got != want {
			t.Fatalf("HeadCommit = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("detached", func(t *testing.T) {
		sandboxtest.Git(t, dir, "checkout", "-q", "--detach", head)
		got, err := sandbox.HeadCommit(dir)
		if err != nil || got != head {
			t.Fatalf("HeadCommit = %q, %v; want %q", got, err, head)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		sandboxtest.WriteFile(t, dir, ".git/HEAD", "ref: refs/heads/nope\n")
		if _, err := sandbox.HeadCommit(dir); err == nil {
			t.Fatal("expected an error for a dangling HEAD")
		}
	})
}

func mustPrepare(t *testing.T, dir string) {
	t.Helper()
	if err := sandbox.PrepareTree(dir); err != nil {
		t.Fatalf("PrepareTree: %v", err)
	}
}
