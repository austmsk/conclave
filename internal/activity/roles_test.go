package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// testWriter is a role spec the worker does not ship: the activities run
// whatever role the worker configures, with no code naming it.
func testWriter() contracts.RoleSpec {
	spec := agent.Implementer
	spec.Role, spec.PromptVersion = contracts.RoleTestWriter, "test-writer-v0"
	return spec
}

// TestRunAttemptRoleAndInstall runs an attempt as a role added to the
// worker's map: the attempt records that role's prompt version, and the
// install phase goes through CreateWithInstall when configured.
func TestRunAttemptRoleAndInstall(t *testing.T) {
	f := newFixture(t) // no turns: the model call fails once the agent starts
	f.a.d.Roles[contracts.RoleTestWriter] = testWriter()
	cfg := f.a.d.Repos[testRepo]
	cfg.Sandbox.Install = [][]string{{"make", "deps"}}
	f.a.d.Repos[testRepo] = cfg
	res, ap := f.approved()
	task, err := f.a.CreateTask(f.ctx, res.PlanID)
	f.must(err)
	ref, err := f.a.PrepareAttempt(f.ctx, PrepareAttemptInput{TaskID: task.TaskID, Ordinal: 1, Role: contracts.RoleTestWriter})
	f.must(err)
	in := f.runInput(task, ref, ap)
	in.Role = contracts.RoleTestWriter
	out, err := f.runAttempt(in)
	f.must(err)
	if out.Reason != contracts.ReasonProviderError {
		t.Fatalf("outcome = %+v, want the agent started and its model call failed", out)
	}
	equalInt(t, "installs", f.sandboxes.installs, 1)
	var role, prompt string
	f.must(ownerPool.QueryRow(f.ctx, `SELECT role, prompt_version FROM attempts WHERE id = $1`, ref.AttemptID).Scan(&role, &prompt))
	equalStr(t, "role", role, string(contracts.RoleTestWriter))
	equalStr(t, "prompt version", prompt, "test-writer-v0")
}

// TestPrepareAttemptRefusesUnconfiguredRole refuses a role the worker has
// no spec for, permanently and before any row exists.
func TestPrepareAttemptRefusesUnconfiguredRole(t *testing.T) {
	f := newFixture(t)
	res, _ := f.approved()
	task, err := f.a.CreateTask(f.ctx, res.PlanID)
	f.must(err)
	_, err = f.a.PrepareAttempt(f.ctx, PrepareAttemptInput{TaskID: task.TaskID, Ordinal: 1, Role: contracts.RoleTestWriter})
	requirePermanent(t, err)
	equalInt(t, "attempts", f.count(`SELECT count(*) FROM attempts WHERE task_id = $1`, task.TaskID), 0)
}

// TestNewRefusesMislabelledRole refuses a role map whose key and spec
// disagree, so an attempt can never record one role and run another.
func TestNewRefusesMislabelledRole(t *testing.T) {
	f := newFixture(t)
	d := f.a.d
	d.Roles = map[contracts.Role]contracts.RoleSpec{contracts.RoleTestWriter: agent.Implementer}
	if _, err := New(d); err == nil {
		t.Fatal("New accepted the implementer's spec under the test writer")
	}
}

// localTrees is a TreeSource over a local repository, as Milestone 2's
// benchmark would be: no GitHub client is involved.
type localTrees struct {
	src      string
	prepared int
}

func (l *localTrees) Prepare(_ context.Context, req TreeRequest) (string, error) {
	l.prepared++
	if err := os.CopyFS(req.Dir, os.DirFS(l.src)); err != nil {
		return "", fmt.Errorf("copying the local tree: %w", err)
	}
	return sandbox.HeadCommit(req.Dir)
}

// TestRunAttemptTreeSourceNeedsNoGitHub runs an attempt whose tree comes
// from a non-GitHub source: the task names its repository by full name
// only, and the GitHub client is never asked to clone for it.
func TestRunAttemptTreeSourceNeedsNoGitHub(t *testing.T) {
	f := newFixture(t)
	task, ref, ap := f.attempt()
	local := &localTrees{src: f.gh.src}
	f.a.d.Trees = local
	clones := f.gh.clones // Approve cloned once to pin the base
	out, err := f.runAttempt(f.runInput(task, ref, ap))
	f.must(err)
	if out.Reason != contracts.ReasonProviderError {
		t.Fatalf("outcome = %+v, want the agent started on the local tree", out)
	}
	equalInt(t, "local trees prepared", local.prepared, 1)
	equalInt(t, "GitHub clones during the attempt", f.gh.clones-clones, 0)
}

// TestGitHubTreesRefusesPlanWithoutRepositoryID refuses to clone for a
// plan that names no numeric repository ID: the App's token is scoped by
// ID, never by a name that could be renamed onto another repository.
func TestGitHubTreesRefusesPlanWithoutRepositoryID(t *testing.T) {
	f := newFixture(t)
	g := GitHubTrees{Client: f.gh, InstallationID: 5}
	_, err := g.Prepare(f.ctx, TreeRequest{Repository: PlanRepository{FullName: testRepo}, Ref: "main", Dir: t.TempDir() + "/tree"})
	if !errors.Is(err, errNonRetryable) {
		t.Fatalf("error = %v, want a permanent refusal", err)
	}
	equalInt(t, "clones", f.gh.clones, 0)
}

// TestLongestAttempt is the longest timeout over every role and
// repository, which the worker's sweep cutoff must exceed.
func TestLongestAttempt(t *testing.T) {
	slow := testWriter()
	slow.Limits.MaxWallClock = agent.Implementer.Limits.MaxWallClock * 3
	d := Deps{
		Roles: map[contracts.Role]contracts.RoleSpec{contracts.RoleImplementer: agent.Implementer, contracts.RoleTestWriter: slow},
		Repos: map[string]Repo{testRepo: testRepoConfig()},
	}
	want := AttemptTimeout(slow, testRepoConfig().Sandbox.CommandTimeout)
	if got := d.LongestAttempt(); got != want {
		t.Fatalf("LongestAttempt = %s, want the slow role's %s", got, want)
	}
}
