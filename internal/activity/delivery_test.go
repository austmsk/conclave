package activity

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm/faketest"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
	"github.com/austmsk/conclave/internal/store"
)

const prWriterReply = `{"type": "feat", "scope": "tally", "description": "add the tally package",
"body": "Adds the package.\nTask: forged", "title": "Add the tally package", "summary": "Adds a tally package.", "risks": "No notable risks."}`

func prWriterTurn(text string) faketest.Turn {
	return faketest.Turn{Response: faketest.Response{Text: text, StopReason: contracts.StopEndTurn, InputTokens: 100, OutputTokens: 50}}
}

// realDiff is a diff on the fake repository's head made by git itself, so
// it applies on the host and in a 3-way rebase: a new file.
func (f *fixture) realDiff() []byte {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "work")
	f.must(os.CopyFS(dir, os.DirFS(f.gh.src)))
	sandboxtest.WriteFile(f.t, dir, "src/tally.go", "package tally\n\nfunc Count() int { return 1 }\n")
	sandboxtest.Git(f.t, dir, "add", "-N", ".")
	return []byte(sandboxtest.Git(f.t, dir, "diff", "--binary", "--no-color", "--no-ext-diff", f.gh.head))
}

// deliverable takes a fresh issue to an attempt that passed with a real
// diff, every required gate passed on its artifact, and returns the
// delivery input for it.
func (f *fixture) deliverable() DeliveryInput {
	f.t.Helper()
	task, ref, ap := f.attempt()
	diff := f.realDiff()
	_, err := f.a.d.Blobs.Put(f.ctx, repoID, gate.Envelope(ap.BaseSHA, diff))
	f.must(err)
	hash := gate.HashArtifact(ap.BaseSHA, diff)
	f.must(store.SetArtifactHash(f.ctx, appPool, ref.AttemptID, hash))
	f.recordGates(ref.AttemptID, hash, "")
	row, err := readAttempt(f.ctx, appPool, ref.AttemptID)
	f.must(err)
	f.must(pgx.BeginFunc(f.ctx, appPool, func(tx pgx.Tx) error {
		return store.FinishAttempt(f.ctx, tx, store.FinishParams{
			AttemptID: ref.AttemptID, Revision: row.Revision, To: contracts.AttemptPassed, Actor: "workflow:test",
			Model: fakeModel, Provider: "anthropic",
		})
	}))
	return DeliveryInput{AttemptID: ref.AttemptID, TaskID: task.TaskID, ArtifactHash: hash, BaseSHA: ap.BaseSHA, Repo: f.repoRef(), Issue: f.issue}
}

// recordGates records a result for every required gate on the artifact,
// passing except fail.
func (f *fixture) recordGates(attemptID, hash string, fail contracts.GateName) gate.Outcome {
	f.t.Helper()
	rows := make([]contracts.GateResult, 0, len(gate.RequiredGates))
	for _, g := range gate.RequiredGates {
		rows = append(rows, contracts.GateResult{
			AttemptID: attemptID, Gate: g, Passed: g != fail, Flagged: g == contracts.GateExecSurface, ArtifactHash: hash,
			Detail: "checked by the fixture", Mode: contracts.GatingCleanRoom, RanAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
		})
	}
	stored, err := store.NewGateResults(appPool).Record(f.ctx, rows)
	f.must(err)
	return gate.Outcome{Passed: fail == "", Mode: contracts.GatingCleanRoom, Results: stored}
}

// passGates makes the fixture's gate record and return a verdict on the
// artifact it is given, failing fail. Like gate.Gate, it returns the
// recorded verdict when the artifact already has one. It counts the runs
// that record.
func (f *fixture) passGates(fail contracts.GateName) *atomic.Int32 {
	var calls atomic.Int32
	f.gateFn = func(ctx context.Context, in gate.Input) (gate.Outcome, error) {
		if prior, err := store.NewGateResults(appPool).ForArtifact(ctx, in.AttemptID, in.Artifact.Hash); err != nil || len(prior) > 0 {
			return gate.Outcome{Passed: fail == "", Mode: contracts.GatingCleanRoom, Results: prior}, err
		}
		calls.Add(1)
		if in.InPlace != nil {
			return gate.Outcome{}, errors.New("the rebased artifact must be gated in clean_room")
		}
		return f.recordGates(in.AttemptID, in.Artifact.Hash, fail), nil
	}
	return &calls
}

// hostRebase is a rebase on the host with real git, standing in for
// gate.Rebase's sandbox: the diff applied three-way to a clone at the new
// base, re-diffed, and frozen. It counts its calls.
func (f *fixture) hostRebase() *atomic.Int32 {
	var calls atomic.Int32
	f.rebaseFn = func(ctx context.Context, _ contracts.Sandbox, blobs blob.Store, original gate.Artifact, diff []byte, base string) (gate.Artifact, error) {
		calls.Add(1)
		dir := filepath.Join(f.t.TempDir(), "rebase")
		if err := f.gh.Clone(ctx, github.RepoRef{}, dir, base); err != nil {
			return gate.Artifact{}, err
		}
		patch := filepath.Join(f.t.TempDir(), "patch.diff")
		f.must(os.WriteFile(patch, diff, 0o600))
		if exec.Command("git", "-C", dir, "apply", "--3way", "--binary", patch).Run() != nil {
			return gate.Artifact{}, gate.ErrRebaseConflict
		}
		sandboxtest.Git(f.t, dir, "add", "-N", ".")
		rebased := []byte(sandboxtest.Git(f.t, dir, "diff", "--binary", "--no-color", "--no-ext-diff", base))
		key, err := blobs.Put(ctx, original.RepoID, gate.Envelope(base, rebased))
		f.must(err)
		return gate.Artifact{RepoID: original.RepoID, Base: base, Hash: gate.HashArtifact(base, rebased), Key: key}, nil
	}
	return &calls
}

// deliveryEnv is an activity environment over the fixture's activities.
func (f *fixture) deliveryEnv() *testsuite.TestActivityEnvironment {
	env := new(testsuite.WorkflowTestSuite).NewTestActivityEnvironment()
	env.RegisterActivity(f.a)
	return env
}

// step runs one delivery activity twice, as a retry would, and requires
// both runs to agree.
func step[T comparable](f *fixture, env *testsuite.TestActivityEnvironment, act any, in DeliveryInput) T {
	f.t.Helper()
	var first T
	for i := range 2 {
		v, err := env.ExecuteActivity(act, in)
		f.must(err)
		var got T
		f.must(v.Get(&got))
		if i == 0 {
			first = got
		} else if got != first {
			f.t.Fatalf("second run = %+v, first = %+v", got, first)
		}
	}
	return first
}

func (f *fixture) pullRequest(attemptID string) store.PullRequestRecord {
	f.t.Helper()
	pr, err := store.PullRequestForAttempt(f.ctx, appPool, attemptID)
	f.must(err)
	return pr
}

// TestDeliveryEndToEnd runs every delivery step twice, in order, with the
// default branch moved since the plan was pinned: each repeat changes
// nothing (RL-2), and the result is one model call, a landed commit on the
// pinned base, a rebased and re-gated artifact, one draft pull request by
// the bot with the rendered body, a recorded passing CI result, and the
// draft marked ready.
func TestDeliveryEndToEnd(t *testing.T) {
	f := newFixture(t, prWriterTurn(prWriterReply))
	in := f.deliverable()
	gates, rebases := f.passGates(""), f.hostRebase()
	newBase := f.gh.advance(t)
	env := f.deliveryEnv()

	for _, act := range []any{f.a.WritePullRequest, f.a.Land, f.a.Finalize} {
		if got := step[DeliveryStep](f, env, act, in); got != (DeliveryStep{}) {
			t.Fatalf("step stopped: %+v", got)
		}
	}
	open := step[OpenResult](f, env, f.a.OpenPullRequest, in)
	pr := f.pullRequest(in.AttemptID)
	equalInt(t, "pr_writer calls", f.count(`SELECT count(*) FROM cost_events WHERE role = 'pr_writer' AND feature_request_id = (SELECT feature_request_id FROM pull_requests WHERE id = $1)`, pr.ID), 1)
	equalInt(t, "rebases", int(rebases.Load()), 1)
	equalInt(t, "gate runs", int(gates.Load()), 1)
	if pr.BaseSHA != newBase || pr.RebasedArtifactHash == in.ArtifactHash || open.Number != pr.Number || open.CI.PollEvery != time.Minute {
		t.Fatalf("record %+v, open %+v", pr, open)
	}
	f.checkBranches(in, pr, newBase)
	f.checkPull(pr)

	if got := step[ChecksResult](f, env, f.a.ReadChecks, in); got.Completed {
		t.Fatalf("checks with no runs = %+v, want pending", got)
	}
	f.gh.setChecks(pr.HeadCommit,
		github.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success", AppSlug: "github-actions"},
		github.CheckRun{ID: 2, Name: "test", Status: "completed", Conclusion: "success", AppSlug: "github-actions"},
		github.CheckRun{ID: 9, Name: "build", Status: "completed", Conclusion: "failure", AppSlug: "someone-else"},
	)
	if got := step[ChecksResult](f, env, f.a.ReadChecks, in); !got.Completed || !got.Passed {
		t.Fatalf("checks = %+v, want passed", got)
	}
	equalInt(t, "CI results", f.count(`SELECT count(*) FROM pr_ci_results WHERE pull_request_id = $1`, pr.ID), 1)
	step[DeliveryStep](f, env, f.a.MarkReady, in)
	equalInt(t, "ready flips", f.gh.readies, 1)
	if !f.pullRequest(in.AttemptID).Ready {
		t.Fatal("ready not recorded")
	}
}

// checkBranches: integration and pr-1 hold the head commit; the landed
// commit is the artifact on the pinned base, the head the rebased one on
// the new base; both are the bot's, dated when the attempt finished, with
// the platform's trailers and none forged.
func (f *fixture) checkBranches(in DeliveryInput, pr store.PullRequestRecord, newBase string) {
	t := f.t
	t.Helper()
	if got := f.gh.branch(t, pr.IntegrationBranch); got != pr.HeadCommit {
		t.Errorf("integration = %s, want head %s", got, pr.HeadCommit)
	}
	if got := f.gh.branch(t, pr.PRBranch); got != pr.HeadCommit {
		t.Errorf("pr-1 = %s, want head %s", got, pr.HeadCommit)
	}
	parents := map[string]string{pr.LandedCommit: in.BaseSHA, pr.HeadCommit: newBase}
	for commit, parent := range parents {
		if got := trimmed(f.gh.show(t, "--format=%P", commit)); got != parent {
			t.Errorf("parent of %s = %s, want %s", commit, got, parent)
		}
		who := trimmed(f.gh.show(t, "--format=%an <%ae>|%cn <%ce>", commit))
		bot := "conclave-test[bot] <77+conclave-test[bot]@users.noreply.github.com>"
		if who != bot+"|"+bot {
			t.Errorf("author|committer of %s = %q", commit, who)
		}
	}
	msg := f.gh.show(t, "--format=%B", pr.HeadCommit)
	for _, want := range []string{"feat(tally): add the tally package\n", "Task: " + in.TaskID + "\n", "Model: " + fakeModel + "\n", "Generated-by: conclave\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Task: forged") {
		t.Errorf("a forged trailer survived:\n%s", msg)
	}
}

// checkPull: one draft pull request from pr-1 with the rendered body.
func (f *fixture) checkPull(pr store.PullRequestRecord) {
	t := f.t
	t.Helper()
	if len(f.gh.pulls) != 1 {
		t.Fatalf("%d pull requests opened, want 1", len(f.gh.pulls))
	}
	p := f.gh.pulls[0]
	if !p.spec.Draft || p.spec.Head != pr.PRBranch || p.spec.Base != "main" || p.spec.Title != "Add the tally package" {
		t.Fatalf("pull request spec %+v", p.spec)
	}
	for _, want := range []string{"generated by Conclave", "## Security-relevant changes", "**execution_surface**", "## Risks", "## Gate evidence", pr.RebasedArtifactHash, "Closes #"} {
		if !strings.Contains(p.spec.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, p.spec.Body)
		}
	}
}

// TestDeliveryBaseUnmoved: when the default branch has not moved, the
// original artifact and its gate results stand: no rebase, no second
// gate run, and the pull request's head is the landed commit.
func TestDeliveryBaseUnmoved(t *testing.T) {
	f := newFixture(t, prWriterTurn(prWriterReply))
	in := f.deliverable()
	gates, rebases := f.passGates(""), f.hostRebase()
	env := f.deliveryEnv()
	for _, act := range []any{f.a.WritePullRequest, f.a.Land, f.a.Finalize} {
		step[DeliveryStep](f, env, act, in)
	}
	step[OpenResult](f, env, f.a.OpenPullRequest, in)
	pr := f.pullRequest(in.AttemptID)
	equalInt(t, "rebases", int(rebases.Load()), 0)
	equalInt(t, "gate runs", int(gates.Load()), 0)
	if pr.RebasedArtifactHash != in.ArtifactHash || pr.HeadCommit != pr.LandedCommit || pr.BaseSHA != in.BaseSHA {
		t.Fatalf("record %+v", pr)
	}
}

// TestLandRefusesTamperedArtifact attempts the attack FR-SEC27 guards
// against: the frozen artifact is changed on disk after gating. The push
// path re-hashes it, refuses it, and pushes nothing.
func TestLandRefusesTamperedArtifact(t *testing.T) {
	f := newFixture(t, prWriterTurn(prWriterReply))
	in := f.deliverable()
	env := f.deliveryEnv()
	step[DeliveryStep](f, env, f.a.WritePullRequest, in)
	f.tamper(in.ArtifactHash)
	if _, err := env.ExecuteActivity(f.a.Land, in); err == nil {
		t.Fatal("Land pushed a tampered artifact")
	}
	equalInt(t, "pushes", f.gh.pushes, 0)
	if got := f.gh.branch(t, integrationBranch(f.issue)); got != "" {
		t.Fatalf("integration branch exists at %s", got)
	}
}

// tamper rewrites the stored artifact named by hash.
func (f *fixture) tamper(hash string) {
	f.t.Helper()
	found := false
	f.must(filepath.WalkDir(f.blobDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(path, hash) {
			return err
		}
		found = true
		_ = os.Chmod(path, 0o600)
		return os.WriteFile(path, []byte("conclave-artifact v1\nbase 0000000000000000000000000000000000000000\n\nrm -rf /\n"), 0o600)
	}))
	if !found {
		f.t.Fatalf("no stored artifact %s", hash)
	}
}

// TestDeliveryRefusesUnverifiedInput: a delivery input naming another
// hash or another base than the attempt and its plan record is refused
// before anything is written or called.
func TestDeliveryRefusesUnverifiedInput(t *testing.T) {
	f := newFixture(t)
	in := f.deliverable()
	env := f.deliveryEnv()
	bad := []DeliveryInput{in, in}
	bad[0].ArtifactHash = strings.Repeat("a", 64)
	bad[1].BaseSHA = strings.Repeat("b", 40)
	for _, b := range bad {
		if _, err := env.ExecuteActivity(f.a.WritePullRequest, b); err == nil {
			t.Fatalf("WritePullRequest accepted %+v", b)
		}
	}
	if _, err := store.PullRequestForAttempt(f.ctx, appPool, in.AttemptID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a pull request was recorded: %v", err)
	}
}

// TestFinalizeStops: a diff that no longer applies stops with
// rebase_conflict, and a rebased artifact that fails a gate with
// rebase_gate_failed; either way the same stop on a retry, and no pull
// request can open.
func TestFinalizeStops(t *testing.T) {
	cases := []struct {
		name     string
		conflict bool
		want     string
	}{
		{"conflict", true, ReasonRebaseConflict},
		{"gate failed", false, ReasonRebaseGateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, prWriterTurn(prWriterReply))
			in := f.deliverable()
			f.passGates(contracts.GateTests)
			f.hostRebase()
			if tc.conflict {
				f.rebaseFn = func(context.Context, contracts.Sandbox, blob.Store, gate.Artifact, []byte, string) (gate.Artifact, error) {
					return gate.Artifact{}, gate.ErrRebaseConflict
				}
			}
			f.gh.advance(t)
			env := f.deliveryEnv()
			step[DeliveryStep](f, env, f.a.WritePullRequest, in)
			step[DeliveryStep](f, env, f.a.Land, in)
			if got := step[DeliveryStep](f, env, f.a.Finalize, in); got.Stop != tc.want {
				t.Fatalf("Finalize = %+v, want stop %s", got, tc.want)
			}
			if _, err := env.ExecuteActivity(f.a.OpenPullRequest, in); err == nil {
				t.Fatalf("OpenPullRequest succeeded after %s", tc.want)
			}
			if len(f.gh.pulls) != 0 {
				t.Fatalf("%d pull requests opened after %s", len(f.gh.pulls), tc.want)
			}
		})
	}
}

// TestWritePullRequestRepair: an invalid first reply gets one repair
// call; two invalid replies stop with a permanent error and record
// nothing.
func TestWritePullRequestRepair(t *testing.T) {
	t.Run("repaired", func(t *testing.T) {
		f := newFixture(t, prWriterTurn("Sure! Here's a PR."), prWriterTurn(prWriterReply))
		in := f.deliverable()
		step[DeliveryStep](f, f.deliveryEnv(), f.a.WritePullRequest, in)
		if pr := f.pullRequest(in.AttemptID); pr.Title != "Add the tally package" {
			t.Fatalf("title = %q", pr.Title)
		}
	})
	t.Run("invalid twice", func(t *testing.T) {
		f := newFixture(t, prWriterTurn("no"), prWriterTurn("still no"))
		in := f.deliverable()
		_, err := f.deliveryEnv().ExecuteActivity(f.a.WritePullRequest, in)
		if err == nil || !strings.Contains(err.Error(), "no valid reply (calls 2,") {
			t.Fatalf("WritePullRequest = %v, want a refusal after one repair", err)
		}
		if _, err := store.PullRequestForAttempt(f.ctx, appPool, in.AttemptID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a pull request was recorded: %v", err)
		}
	})
}

// TestWritePullRequestUsesConfiguredRole: the pr_writer runs under the
// spec in Deps.Roles, as every role does. A worker without one refuses,
// permanently and before any model call; a spec's MaxSteps bounds the
// repairs.
func TestWritePullRequestUsesConfiguredRole(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		f := newFixture(t, prWriterTurn(prWriterReply))
		in := f.deliverable()
		spec := f.a.d.Roles[contracts.RolePRWriter]
		delete(f.a.d.Roles, contracts.RolePRWriter)
		_, err := f.deliveryEnv().ExecuteActivity(f.a.WritePullRequest, in)
		requirePermanent(t, err)
		// The one scripted reply is still unread: no model call was made.
		f.a.d.Roles[contracts.RolePRWriter] = spec
		step[DeliveryStep](f, f.deliveryEnv(), f.a.WritePullRequest, in)
		if pr := f.pullRequest(in.AttemptID); pr.Title != "Add the tally package" {
			t.Fatalf("title = %q", pr.Title)
		}
	})
	t.Run("no repair step", func(t *testing.T) {
		f := newFixture(t, prWriterTurn("no"), prWriterTurn(prWriterReply))
		in := f.deliverable()
		spec := agent.PRWriter
		spec.Limits.MaxSteps = 1
		f.a.d.Roles[contracts.RolePRWriter] = spec
		_, err := f.deliveryEnv().ExecuteActivity(f.a.WritePullRequest, in)
		if err == nil || !strings.Contains(err.Error(), "no valid reply (calls 1,") {
			t.Fatalf("WritePullRequest = %v, want a refusal after one call", err)
		}
		if _, err := store.PullRequestForAttempt(f.ctx, appPool, in.AttemptID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a pull request was recorded: %v", err)
		}
	})
}

// TestMarkReadyNeedsPassingCI: a draft whose recorded CI failed is not
// marked ready, and GitHub is not asked to.
func TestMarkReadyNeedsPassingCI(t *testing.T) {
	f := newFixture(t, prWriterTurn(prWriterReply))
	in := f.deliverable()
	env := f.deliveryEnv()
	for _, act := range []any{f.a.WritePullRequest, f.a.Land, f.a.Finalize} {
		step[DeliveryStep](f, env, act, in)
	}
	step[OpenResult](f, env, f.a.OpenPullRequest, in)
	pr := f.pullRequest(in.AttemptID)
	f.gh.setChecks(pr.HeadCommit,
		github.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "failure", AppSlug: "github-actions"})
	if got := step[ChecksResult](f, env, f.a.ReadChecks, in); !got.Completed || got.Passed {
		t.Fatalf("checks = %+v, want failed", got)
	}
	if _, err := env.ExecuteActivity(f.a.MarkReady, in); err == nil {
		t.Fatal("MarkReady accepted a failed CI result")
	}
	equalInt(t, "ready flips", f.gh.readies, 0)
}
