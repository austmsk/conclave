package github_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

const (
	repoName = "austmsk/election-tally"
	branch   = "agent/1/integration"
	// workflowRefusal is GitHub's refusal of an App push that edits a
	// workflow file without the workflows permission.
	workflowRefusal = "refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission"
)

var repo = github.RepoRef{ID: 42, FullName: repoName, InstallationID: 7}

// newApp returns an App whose git traffic goes to gitBase and whose every
// installation token is the canary.
func newApp(t *testing.T, gitBase string) *github.App {
	t.Helper()
	cfg := github.Config{
		AppID:          1,
		AppSlug:        "conclave-test",
		InstallationID: 7,
		OwnerUserID:    3,
		TriggerLabel:   github.DefaultTriggerLabel,
		APIBaseURL:     "http://127.0.0.1:1",
		GitBaseURL:     gitBase,
	}
	app, err := github.NewApp(cfg, githubtest.Key(t), nil)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	github.StubToken(app, githubtest.CanaryToken)
	return app
}

// remote is a bare repository served over smart HTTP, an App pointed at
// it, and a prepared clone of it to push from.
type remote struct {
	root string
	srv  *githubtest.GitServer
	app  *github.App
	head string
	tree string
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	root := t.TempDir()
	r := &remote{root: root, srv: githubtest.NewGitServer(t, root, githubtest.CanaryToken)}
	r.head = githubtest.NewBareRepo(t, root, repoName)
	r.app = newApp(t, r.srv.URL)
	r.tree = filepath.Join(t.TempDir(), "clone")
	if err := r.app.Clone(t.Context(), repo, r.tree, "main"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	return r
}

func (r *remote) branchHead(t *testing.T) string {
	t.Helper()
	return githubtest.RefValue(t, r.root, repoName, "refs/heads/"+branch)
}

func (r *remote) push(t *testing.T, commit, expectedOld string) error {
	t.Helper()
	return r.app.Push(t.Context(), github.PushSpec{Repo: repo, Dir: r.tree, Commit: commit, Branch: branch, ExpectedOld: expectedOld})
}

// commit commits a change to the prepared tree and prepares it again,
// since committing writes a reflog VerifyTree refuses. It returns the
// commit.
func commit(t *testing.T, dir, content string) string {
	t.Helper()
	sandboxtest.WriteFile(t, dir, "CHANGES.md", content)
	sandboxtest.Git(t, dir, "add", "-A")
	sandboxtest.Git(t, dir, "commit", "-q", "-m", content)
	sha := strings.TrimSpace(sandboxtest.Git(t, dir, "rev-parse", "HEAD"))
	if err := sandbox.PrepareTree(dir); err != nil {
		t.Fatalf("PrepareTree: %v", err)
	}
	return sha
}

func wantPushError(t *testing.T, err error, reason github.PushFailure) (detail string) {
	t.Helper()
	var pe *github.PushError
	if !errors.As(err, &pe) {
		t.Fatalf("Push = %v, want a *PushError", err)
	}
	if pe.Reason != reason {
		t.Fatalf("Push reason = %q (%v), want %q", pe.Reason, err, reason)
	}
	return pe.Detail
}

func TestCheckBranch(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"agent/1/integration", true},
		{"agent/42/pr-3", true},
		{"main", false},
		{"refs/heads/main", false},
		{"agent", false},
		{"agent/", false},
		{"agent/0/integration", false},
		{"agent/1/pr-0", false},
		{"agent/1/integration:main", false},
		{"+agent/1/integration", false},
		{"agent/../main", false},
		{"agent/1/integration/x", false},
		{"Agent/1/integration", false},
		{"agent/1/integration\n", false},
		{"-d", false},
		{"аgent/1/integration", false}, // Cyrillic а
		{"agent/1/integ\x00ration", false},
		{"refs/tags/agent/1/integration", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			err := github.CheckBranch(tc.name)
			if tc.ok && err != nil {
				t.Fatalf("CheckBranch = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, github.ErrRefused) {
				t.Fatalf("CheckBranch = %v, want ErrRefused", err)
			}
		})
	}
}

func TestPushOnlyToAgentBranches(t *testing.T) {
	r := newRemote(t)
	c := commit(t, r.tree, "one")
	refused := []string{
		"main", "refs/heads/main", "+agent/1/integration", "agent/../main",
		"agent/1/integration:main", "аgent/1/integration", "-d",
		"agent/1/integ\x00ration", "refs/tags/agent/1/integration", "",
	}
	for _, b := range refused {
		t.Run(fmt.Sprintf("%q", b), func(t *testing.T) {
			before := r.srv.Requests()
			err := r.app.Push(t.Context(), github.PushSpec{Repo: repo, Dir: r.tree, Commit: c, Branch: b})
			if !errors.Is(err, github.ErrRefused) {
				t.Fatalf("Push = %v, want ErrRefused", err)
			}
			if got := r.srv.Requests() - before; got != 0 {
				t.Fatalf("server saw %d requests for a refused push", got)
			}
		})
	}
}

func TestPushRefusesNonSHACommit(t *testing.T) {
	r := newRemote(t)
	cases := []struct{ name, commit, expectedOld string }{
		{"HEAD", "HEAD", ""},
		{"branch name", "main", ""},
		{"short commit", r.head[:12], ""},
		{"uppercase commit", strings.ToUpper(r.head), ""},
		{"expected old that is a branch", r.head, "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := r.srv.Requests()
			err := r.push(t, tc.commit, tc.expectedOld)
			if !errors.Is(err, github.ErrRefused) {
				t.Fatalf("Push = %v, want ErrRefused", err)
			}
			if got := r.srv.Requests() - before; got != 0 {
				t.Fatalf("server saw %d requests for a refused push", got)
			}
		})
	}
}

func TestPushRefusesUnpreparedTree(t *testing.T) {
	r := newRemote(t)
	cases := []struct {
		name string
		tree func(t *testing.T) (dir, commit string)
	}{
		{"remote, credential helper and extraheader", func(t *testing.T) (string, string) { return sandboxtest.NewRepo(t) }},
		{"hooks directory", func(t *testing.T) (string, string) {
			c := commit(t, r.tree, "hooked")
			sandboxtest.WriteFile(t, r.tree, ".git/hooks/pre-push", "#!/bin/sh\nexit 0\n")
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(r.tree, ".git", "hooks")) })
			return r.tree, c
		}},
		{"no git directory", func(t *testing.T) (string, string) { return t.TempDir(), r.head }},
		{"relative directory", func(t *testing.T) (string, string) { return ".", r.head }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, c := tc.tree(t)
			before := r.srv.Requests()
			err := r.app.Push(t.Context(), github.PushSpec{Repo: repo, Dir: dir, Commit: c, Branch: branch})
			if !errors.Is(err, github.ErrRefused) {
				t.Fatalf("Push = %v, want ErrRefused", err)
			}
			if got := r.srv.Requests() - before; got != 0 {
				t.Fatalf("server saw %d requests for a refused push", got)
			}
		})
	}
}

func TestPushAuthenticatesWithInstallationToken(t *testing.T) {
	r := newRemote(t)
	c := commit(t, r.tree, "one")
	before := r.srv.Authorized()
	if err := r.push(t, c, ""); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := r.branchHead(t); got != c {
		t.Errorf("branch = %q, want %q", got, c)
	}
	if r.srv.Authorized() == before || r.srv.Rejected() != 0 {
		t.Errorf("server saw %d authorized and %d rejected requests; every request must carry the token",
			r.srv.Authorized()-before, r.srv.Rejected())
	}
}

func TestPushRepeatedIsIdempotent(t *testing.T) {
	r := newRemote(t)
	c := commit(t, r.tree, "one")
	if err := r.push(t, c, ""); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	// A retry of a push that succeeded unreported carries the same lease.
	if err := r.push(t, c, ""); err != nil {
		t.Fatalf("repeated Push = %v, want nil", err)
	}
	if got := r.branchHead(t); got != c {
		t.Errorf("branch = %q, want %q", got, c)
	}
}

func TestPushNewBranchMustNotExist(t *testing.T) {
	r := newRemote(t)
	first := commit(t, r.tree, "one")
	if err := r.push(t, first, ""); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	second := commit(t, r.tree, "two")
	wantPushError(t, r.push(t, second, ""), github.PushStaleLease)
	if got := r.branchHead(t); got != first {
		t.Errorf("branch = %q, want it left at %q", got, first)
	}
}

func TestPushLeaseRefusesStaleExpected(t *testing.T) {
	r := newRemote(t)
	first := commit(t, r.tree, "one")
	if err := r.push(t, first, ""); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	second := commit(t, r.tree, "two")
	wantPushError(t, r.push(t, second, r.head), github.PushStaleLease)
	if got := r.branchHead(t); got != first {
		t.Errorf("branch = %q, want it left at %q", got, first)
	}
}

func TestPushLeaseAllowsMatchingForce(t *testing.T) {
	r := newRemote(t)
	first := commit(t, r.tree, "one")
	if err := r.push(t, first, ""); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	// A sibling of first: replacing first with it is not a fast-forward.
	sandboxtest.Git(t, r.tree, "checkout", "-q", "--detach", r.head)
	sibling := commit(t, r.tree, "sibling")
	if err := r.push(t, sibling, first); err != nil {
		t.Fatalf("Push with a matching lease: %v", err)
	}
	if got := r.branchHead(t); got != sibling {
		t.Errorf("branch = %q, want %q", got, sibling)
	}
}

func TestPushRemoteRejectionClassified(t *testing.T) {
	r := newRemote(t)
	githubtest.RejectPushes(t, r.root, repoName, workflowRefusal)
	c := commit(t, r.tree, "one")
	detail := wantPushError(t, r.push(t, c, ""), github.PushRemoteRejected)
	if !strings.Contains(detail, workflowRefusal) {
		t.Errorf("Detail = %q, want it to carry %q", detail, workflowRefusal)
	}
	if len(detail) > 200 {
		t.Errorf("Detail is %d bytes, want at most 200", len(detail))
	}
}

func TestPushErrorRedacted(t *testing.T) {
	r := newRemote(t)
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubtest.CanaryToken))
	githubtest.RejectPushes(t, r.root, repoName, "denied for "+githubtest.CanaryToken+" "+header)
	c := commit(t, r.tree, "one")
	err := r.push(t, c, "")
	detail := wantPushError(t, err, github.PushRemoteRejected)
	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(text, githubtest.CanaryToken) || strings.Contains(text, header) {
			t.Errorf("error text carries the token: %s", text)
		}
	}
	if !strings.Contains(detail, "[redacted]") {
		t.Errorf("Detail = %q, want the token replaced by [redacted]", detail)
	}
}

func TestPushDoesNotFollowRedirect(t *testing.T) {
	r := newRemote(t)
	c := commit(t, r.tree, "one")
	redirect := githubtest.NewRedirectServer(t, r.srv.URL)
	app := newApp(t, redirect.URL)
	before := r.srv.Requests()
	err := app.Push(t.Context(), github.PushSpec{Repo: repo, Dir: r.tree, Commit: c, Branch: branch})
	if err == nil {
		t.Fatal("Push through a redirect succeeded")
	}
	if redirect.Requests() == 0 {
		t.Fatal("redirect server saw no request; the test proves nothing")
	}
	if got := r.srv.Requests() - before; got != 0 {
		t.Errorf("redirect target saw %d requests", got)
	}
	if strings.Contains(err.Error(), githubtest.CanaryToken) {
		t.Errorf("error carries the token: %v", err)
	}
}

func TestPushGitSeesNoInheritedEnv(t *testing.T) {
	tree := t.TempDir()
	sandboxtest.Git(t, tree, "init", "-q", "-b", "main", ".")
	c := commit(t, tree, "one")

	// A fake git, first on PATH, records what it was given and fails.
	bin := t.TempDir()
	envFile, argsFile := filepath.Join(bin, "env.txt"), filepath.Join(bin, "args.txt")
	script := fmt.Sprintf("#!/bin/sh\nenv > '%s'\nprintf '%%s\\n' \"$@\" > '%s'\nexit 1\n", envFile, argsFile)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CONCLAVE_TEST_INHERITED", "canary_inherited_variable")

	app := newApp(t, "http://127.0.0.1:1")
	if err := app.Push(t.Context(), github.PushSpec{Repo: repo, Dir: tree, Commit: c, Branch: branch}); err == nil {
		t.Fatal("Push succeeded with a failing git")
	}
	env, args := readFile(t, envFile), readFile(t, argsFile)
	if strings.Contains(env, "canary_inherited_variable") {
		t.Error("git inherited the worker's environment")
	}
	if !strings.Contains(env, "HOME=/nonexistent\n") {
		t.Errorf("git ran without HOME=/nonexistent:\n%s", env)
	}
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubtest.CanaryToken))
	if strings.Contains(args, githubtest.CanaryToken) || strings.Contains(args, header) {
		t.Errorf("token on git's command line:\n%s", args)
	}
	if strings.Contains(env, githubtest.CanaryToken) {
		t.Error("raw token in git's environment; only the header form belongs there")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake git left no record: %v", err)
	}
	return string(data)
}

func TestCloneUsesInstallationToken(t *testing.T) {
	r := newRemote(t)
	if r.srv.Authorized() == 0 || r.srv.Rejected() != 0 {
		t.Fatalf("server saw %d authorized and %d rejected requests; every request must carry the token",
			r.srv.Authorized(), r.srv.Rejected())
	}
	if got, err := sandbox.HeadCommit(r.tree); err != nil || got != r.head {
		t.Errorf("HeadCommit = %q, %v; want %q", got, err, r.head)
	}
}

func TestCloneLeavesNoToken(t *testing.T) {
	r := newRemote(t)
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubtest.CanaryToken))
	if hits := filesContaining(t, r.tree, githubtest.CanaryToken, header); len(hits) != 0 {
		t.Errorf("token written to %v", hits)
	}
	if err := sandbox.VerifyTree(r.tree); err != nil {
		t.Errorf("clone is not a prepared tree: %v", err)
	}
}

func TestCloneRefusesBadRepositoryName(t *testing.T) {
	app := newApp(t, "http://127.0.0.1:1")
	for _, name := range []string{"austmsk/../etc", "austmsk/x?y", "user@host/repo", "austmsk/repo.git"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clone")
			err := app.Clone(t.Context(), github.RepoRef{ID: 1, FullName: name}, dir, "main")
			if !errors.Is(err, github.ErrRefused) {
				t.Fatalf("Clone = %v, want ErrRefused", err)
			}
		})
	}
}

// filesContaining lists the files under root whose contents hold any of
// needles.
func filesContaining(t *testing.T, root string, needles ...string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, n := range needles {
			if strings.Contains(string(data), n) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}
