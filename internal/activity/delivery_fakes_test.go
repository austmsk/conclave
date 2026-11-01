package activity

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// fakePull is a pull request the fake holds.
type fakePull struct {
	spec   github.PullRequestSpec
	number int
	draft  bool
}

// Push pushes into the fake's bare remote with real git and the same
// lease the App sends, so a stale lease and a repeated push behave as
// they do against GitHub. The spec is checked as the App checks it.
func (g *fakeGitHub) Push(ctx context.Context, p github.PushSpec) error {
	if err := github.CheckBranch(p.Branch); err != nil {
		return err
	}
	if err := sandbox.VerifyTree(p.Dir); err != nil {
		return fmt.Errorf("%w: %w", github.ErrRefused, err)
	}
	ref := "refs/heads/" + p.Branch
	cmd := exec.CommandContext(ctx, "git", "-C", p.Dir, "push", "--porcelain",
		"--force-with-lease="+ref+":"+p.ExpectedOld, g.remote, p.Commit+":"+ref)
	out, err := cmd.CombinedOutput()
	g.mu.Lock()
	g.pushes++
	g.mu.Unlock()
	if err == nil && !strings.Contains(string(out), "\n!\t") && !strings.HasPrefix(string(out), "!\t") {
		return nil
	}
	if strings.Contains(string(out), "stale info") {
		return &github.PushError{Branch: p.Branch, Reason: github.PushStaleLease}
	}
	return &github.PushError{Branch: p.Branch, Reason: github.PushOther, Detail: strings.TrimSpace(string(out))}
}

// branch reads a branch of the fake's remote, "" when it does not exist.
func (g *fakeGitHub) branch(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", g.remote, "rev-parse", "--verify", "-q", "refs/heads/"+name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// show runs git show in the remote and returns its output.
func (g *fakeGitHub) show(t *testing.T, args ...string) string {
	t.Helper()
	return sandboxtest.Git(t, g.remote, append([]string{"show", "-s"}, args...)...)
}

// CreatePullRequest returns the pull request already open from the head,
// as the App does, or opens one.
func (g *fakeGitHub) CreatePullRequest(_ context.Context, p github.PullRequestSpec) (github.PullRequest, error) {
	if err := github.CheckBranch(p.Head); err != nil {
		return github.PullRequest{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pr := range g.pulls {
		if pr.spec.Head == p.Head {
			return pullOf(pr, false), nil
		}
	}
	pr := fakePull{spec: p, number: len(g.pulls) + 1, draft: p.Draft}
	g.pulls = append(g.pulls, pr)
	return pullOf(pr, true), nil
}

func pullOf(p fakePull, created bool) github.PullRequest {
	return github.PullRequest{
		Number: p.number, NodeID: fmt.Sprintf("PR_node_%d", p.number),
		URL: fmt.Sprintf("https://github.example/pull/%d", p.number), Draft: p.draft, Created: created,
	}
}

// ListCheckRuns reports the check runs scripted for a commit.
func (g *fakeGitHub) ListCheckRuns(_ context.Context, _ github.RepoRef, commit string) ([]github.CheckRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]github.CheckRun(nil), g.checkRuns[commit]...), nil
}

// setChecks scripts the check runs on a commit.
func (g *fakeGitHub) setChecks(commit string, runs ...github.CheckRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRuns[commit] = runs
}

// MarkPullRequestReady flips a draft; a pull request already ready is
// returned as it is.
func (g *fakeGitHub) MarkPullRequestReady(_ context.Context, _ github.RepoRef, number int) (github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.pulls {
		if g.pulls[i].number == number {
			if g.pulls[i].draft {
				g.pulls[i].draft = false
				g.readies++
			}
			return pullOf(g.pulls[i], false), nil
		}
	}
	return github.PullRequest{}, fmt.Errorf("pull request %d: %w", number, github.ErrNotFound)
}
