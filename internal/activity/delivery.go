package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/store"
)

// Ticket 9's delivery sequence (decision 0014). The workflow runs, in
// order: WritePullRequest, Land, Finalize, OpenPullRequest, ReadChecks
// on a timer until CI completes, and MarkReady. Each activity is
// idempotent and resumes from the pull_requests row, so a Temporal retry
// or an owner's /retry re-enters at WritePullRequest and repeats no model
// call, push, pull request or cost record. The per-step crash analysis is
// in docs/guide/tickets/09-pull-request-delivery.md.

// BotIdentity is who the platform's commits are authored and committed
// as: the App's bot user (FR-S17).
type BotIdentity struct {
	Name  string
	Email string
}

// delivery is what every delivery step reads before it acts.
type delivery struct {
	in   DeliveryInput
	row  attemptRow
	pc   planContext
	body PlanBody
	cfg  Repo
	pr   store.PullRequestRecord
}

// loadDelivery reads the attempt, its plan, and its repository's
// configuration, and checks the input names what is recorded: a passed
// attempt, its own artifact, its plan's pinned base. With needRow it also
// reads the pull_requests row, which must exist.
func (a *Activities) loadDelivery(ctx context.Context, in DeliveryInput, needRow bool) (delivery, error) {
	d := delivery{in: in}
	var err error
	if d.row, err = readAttempt(ctx, a.d.DB, in.AttemptID); err != nil {
		return delivery{}, err
	}
	if d.pc, err = readPlanContext(ctx, a.d.DB, d.row.TaskID); err != nil {
		return delivery{}, err
	}
	switch {
	case d.row.State != contracts.AttemptPassed:
		return delivery{}, fmt.Errorf("%w: attempt %s is %s, not passed", errNonRetryable, in.AttemptID, d.row.State)
	case d.row.ArtifactHash != in.ArtifactHash:
		return delivery{}, fmt.Errorf("%w: attempt %s recorded artifact %s, not %s", errNonRetryable, in.AttemptID, d.row.ArtifactHash, in.ArtifactHash)
	case d.pc.BaseSHA != in.BaseSHA:
		return delivery{}, fmt.Errorf("%w: plan %s pinned %s, not %s", errNonRetryable, d.pc.PlanID, d.pc.BaseSHA, in.BaseSHA)
	case d.pc.RepoFullName != in.Repo.FullName:
		return delivery{}, fmt.Errorf("%w: attempt %s is in %s, not %s", errNonRetryable, in.AttemptID, d.pc.RepoFullName, in.Repo.FullName)
	}
	if d.body, err = decodePlanBody(d.pc); err != nil {
		return delivery{}, err
	}
	if d.cfg, err = a.repo(in.Repo.FullName); err != nil {
		return delivery{}, err
	}
	if needRow {
		if d.pr, err = store.PullRequestForAttempt(ctx, a.d.DB, in.AttemptID); err != nil {
			return delivery{}, fmt.Errorf("delivering attempt %s: %w", in.AttemptID, err)
		}
	}
	return d, nil
}

// WritePullRequest has the pr_writer write the pull request's title,
// summary, risks and commit subject, builds the commit message with the
// platform's trailers, and records them write-ahead on a new
// pull_requests row.
//
// Idempotent retry: a row already recorded for the attempt is returned
// without calling the model, so a retry or a /retry spends nothing. A
// worker that dies after the model replied and before the insert loses
// that reply; the retry calls again, and both calls are charged, which
// the caps see.
func (a *Activities) WritePullRequest(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	res, err := a.writePullRequest(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) writePullRequest(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	if _, err := store.PullRequestForAttempt(ctx, a.d.DB, in.AttemptID); err == nil {
		return DeliveryStep{}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return DeliveryStep{}, err
	}
	d, err := a.loadDelivery(ctx, in, false)
	if err != nil {
		return DeliveryStep{}, err
	}
	art, err := gate.Verify(ctx, a.d.Blobs, a.gateResults, in.AttemptID, d.pc.RepoID, in.ArtifactHash)
	if err != nil {
		return DeliveryStep{}, err
	}
	out, err := a.callPRWriter(ctx, d, art)
	var budget llm.BudgetError
	if errors.As(err, &budget) {
		return DeliveryStep{Stop: ReasonBudgetCap, Detail: string(budget.Kind)}, nil
	}
	if err != nil {
		return DeliveryStep{}, err
	}
	meta, err := readCommitMeta(ctx, a.d.DB, in.AttemptID, d.pc.PlanID)
	if err != nil {
		return DeliveryStep{}, err
	}
	_, _, err = store.CreatePullRequest(ctx, a.d.DB, store.PullRequestRecord{
		FeatureRequestID: d.pc.FeatureRequestID, TaskID: d.row.TaskID, AttemptID: in.AttemptID,
		IntegrationBranch: integrationBranch(in.Issue), PRBranch: prBranch(in.Issue),
		OriginalBaseSHA: in.BaseSHA, OriginalArtifactHash: in.ArtifactHash,
		Title: out.Title, Summary: out.Summary, Risks: out.Risks,
		CommitMessage: commitMessage(out, d.row.TaskID, d.pc.PlanID, meta),
	})
	return DeliveryStep{}, err
}

// callPRWriter makes the pr_writer's call, and one repair call when the
// reply does not validate, under a feature request scope: the pr_writer
// runs outside any attempt, so the feature request and monthly caps
// bound it (FR-SEC9).
func (a *Activities) callPRWriter(ctx context.Context, d delivery, art gate.Artifact) (agent.PRWriterOutput, error) {
	_, diff, err := gate.Load(ctx, a.d.Blobs, art.Key)
	if err != nil {
		return agent.PRWriterOutput{}, err
	}
	results, err := a.gateResults.ForArtifact(ctx, d.in.AttemptID, art.Hash)
	if err != nil {
		return agent.PRWriterOutput{}, fmt.Errorf("reading gate results of attempt %s: %w", d.in.AttemptID, err)
	}
	report, err := readAttemptReport(ctx, a.d.DB, d.in.AttemptID)
	if err != nil {
		return agent.PRWriterOutput{}, err
	}
	info := activity.GetInfo(ctx)
	scope, err := a.d.Router.FeatureRequestScope(ctx, llm.Scope{
		RepoID: d.pc.RepoID, FeatureRequestID: d.pc.FeatureRequestID,
		KeyPrefix: fmt.Sprintf("%s/%s/%d", info.WorkflowExecution.ID, info.ActivityID, info.Attempt),
	})
	if err != nil {
		return agent.PRWriterOutput{}, fmt.Errorf("scoping the router to feature request %s: %w", d.pc.FeatureRequestID, err)
	}
	req := agent.PRWriterRequest(agent.PRWriterInput{
		RepoID: d.pc.RepoID, Task: d.body.Task, IssueText: d.body.IssueText, Report: report, Diff: diff,
		Gates: gateLines(results),
	})
	resp, err := scope.Complete(ctx, req)
	if err != nil {
		return agent.PRWriterOutput{}, fmt.Errorf("pr_writer call for attempt %s: %w", d.in.AttemptID, err)
	}
	out, perr := agent.ParsePRWriterOutput(resp.Text)
	if perr == nil {
		return out, nil
	}
	resp, err = scope.Complete(ctx, agent.PRWriterRepair(req, resp.Text, perr))
	if err != nil {
		return agent.PRWriterOutput{}, fmt.Errorf("pr_writer repair call for attempt %s: %w", d.in.AttemptID, err)
	}
	out, perr = agent.ParsePRWriterOutput(resp.Text)
	if perr != nil {
		return agent.PRWriterOutput{}, fmt.Errorf("%w: pr_writer for attempt %s after one repair: %w", errNonRetryable, d.in.AttemptID, perr)
	}
	return out, nil
}

// Land pushes the attempt's artifact, as it was gated, as one commit on
// the plan's pinned base to the integration branch (FR-S16): the durable
// record of the task's work. The push path refuses an artifact gate.Verify
// does not accept (FR-SEC27).
//
// Idempotent retry: the commit is deterministic (the stored message, the
// bot, the attempt's finish time), so a retry builds the same SHA, and a
// push of the commit the branch already holds succeeds. The landed commit
// is recorded after the push; a row that records it skips the step.
func (a *Activities) Land(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	res, err := a.land(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) land(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	d, err := a.loadDelivery(ctx, in, true)
	if err != nil || d.pr.LandedCommit != "" {
		return DeliveryStep{}, err
	}
	scratch, err := os.MkdirTemp(a.d.WorkDir, "land-")
	if err != nil {
		return DeliveryStep{}, fmt.Errorf("creating scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	commit, tree, err := a.buildCommit(ctx, d, scratch, in.ArtifactHash)
	if err != nil {
		return DeliveryStep{}, err
	}
	if err := a.push(ctx, in.Repo, tree, commit, d.pr.IntegrationBranch, ""); err != nil {
		return DeliveryStep{}, err
	}
	return DeliveryStep{}, store.SetPullRequestValue(ctx, a.d.DB, d.pr.ID, store.ColLandedCommit, commit)
}

// buildCommit verifies the artifact named by hash, prepares a tree at its
// base, and builds the delivery's commit there: the artifact's diff on
// its base, with the stored message, as the bot, dated when the attempt
// finished. It returns the commit and the tree holding it.
func (a *Activities) buildCommit(ctx context.Context, d delivery, scratch, hash string) (commit, tree string, err error) {
	art, err := gate.Verify(ctx, a.d.Blobs, a.gateResults, d.in.AttemptID, d.pc.RepoID, hash)
	if err != nil {
		return "", "", err
	}
	_, diff, err := gate.Load(ctx, a.d.Blobs, art.Key)
	if err != nil {
		return "", "", err
	}
	meta, err := readCommitMeta(ctx, a.d.DB, d.in.AttemptID, d.pc.PlanID)
	if err != nil {
		return "", "", err
	}
	tree = filepath.Join(scratch, "tree")
	if _, err := a.prepareAt(ctx, d, tree, art.Base); err != nil {
		return "", "", err
	}
	commit, err = sandbox.CommitDiff(ctx, sandbox.CommitSpec{
		Dir: tree, Parent: art.Base, Diff: diff, Message: d.pr.CommitMessage,
		AuthorName: a.d.Bot.Name, AuthorEmail: a.d.Bot.Email, When: meta.FinishedAt,
	})
	if err != nil {
		return "", "", fmt.Errorf("%w: building the commit of attempt %s on %s: %w", errNonRetryable, d.in.AttemptID, art.Base, err)
	}
	return commit, tree, nil
}

// prepareAt prepares the plan's repository at ref in dir and returns its
// head, which must be ref itself when ref is a commit.
func (a *Activities) prepareAt(ctx context.Context, d delivery, dir, ref string) (string, error) {
	head, err := a.d.Trees.Prepare(ctx, TreeRequest{Repository: d.body.Repository, Ref: ref, Dir: dir})
	if err != nil {
		return "", fmt.Errorf("preparing %s at %s: %w", d.in.Repo.FullName, ref, err)
	}
	if ref != d.cfg.Sandbox.DefaultBranch && head != ref {
		return "", fmt.Errorf("%w: tree is at %s, not %s", errNonRetryable, head, ref)
	}
	return head, nil
}

// push pushes commit to branch with the lease expectedOld. A branch that
// already holds commit is success whatever the lease said, so a retry
// after a push that landed unreported succeeds.
func (a *Activities) push(ctx context.Context, repo github.RepoRef, tree, commit, branch, expectedOld string) error {
	err := a.d.GitHub.Push(ctx, github.PushSpec{Repo: repo, Dir: tree, Commit: commit, Branch: branch, ExpectedOld: expectedOld})
	var pe *github.PushError
	if errors.As(err, &pe) && pe.Reason == github.PushStaleLease && expectedOld != commit {
		// The lease may be stale only because an earlier try already
		// moved the branch to commit: push again with that as the lease,
		// which succeeds only if the branch holds exactly commit.
		if again := a.d.GitHub.Push(ctx, github.PushSpec{Repo: repo, Dir: tree, Commit: commit, Branch: branch, ExpectedOld: commit}); again == nil {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("pushing %s to %s: %w", commit, branch, err)
	}
	return nil
}

// Finalize is FR-S19's one rebase at finalization: it records the default
// branch's head as the new base, applies the attempt's diff there in a
// fresh sandbox, freezes the result as a new artifact, and gates it in
// clean_room with gate.Gate unchanged (FR-A5, FR-SEC27). A diff that does
// not apply stops with rebase_conflict; a rebased artifact that fails a
// gate stops with rebase_gate_failed. When the base has not moved, the
// original artifact and its results stand.
//
// Idempotent retry: the new base and the rebased hash are recorded
// write-once as they are learned, so a retry rebases onto the same base
// and, once the hash is recorded, only gates, which returns the recorded
// results when every gate has a row. Nothing leaves the worker.
func (a *Activities) Finalize(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	stop := startHeartbeat(ctx, a.d.HeartbeatEvery, recordHeartbeat)
	defer stop()
	res, err := a.finalize(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) finalize(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	d, err := a.loadDelivery(ctx, in, true)
	if err != nil {
		return DeliveryStep{}, err
	}
	if d.pr.LandedCommit == "" {
		return DeliveryStep{}, fmt.Errorf("%w: attempt %s has not landed", errNonRetryable, in.AttemptID)
	}
	scratch, err := os.MkdirTemp(a.d.WorkDir, "finalize-")
	if err != nil {
		return DeliveryStep{}, fmt.Errorf("creating scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	tree := filepath.Join(scratch, "tree")
	base, err := a.newBase(ctx, d, tree)
	if err != nil {
		return DeliveryStep{}, err
	}
	if base == in.BaseSHA {
		return DeliveryStep{}, store.SetPullRequestValue(ctx, a.d.DB, d.pr.ID, store.ColRebasedArtifactHash, in.ArtifactHash)
	}
	rebased, step, err := a.rebased(ctx, d, tree, base)
	if err != nil || step.Stop != "" {
		return step, err
	}
	return a.gateRebased(ctx, d, tree, rebased)
}

// newBase prepares a tree at the new base in tree and returns the base:
// the one recorded, or else the default branch's head, which it records
// before anything is built on it.
func (a *Activities) newBase(ctx context.Context, d delivery, tree string) (string, error) {
	if d.pr.BaseSHA != "" {
		return a.prepareAt(ctx, d, tree, d.pr.BaseSHA)
	}
	head, err := a.prepareAt(ctx, d, tree, d.cfg.Sandbox.DefaultBranch)
	if err != nil {
		return "", err
	}
	if err := store.SetPullRequestValue(ctx, a.d.DB, d.pr.ID, store.ColBaseSHA, head); err != nil {
		return "", err
	}
	return head, nil
}

// rebased returns the rebased artifact: the one recorded, or a new one
// made in a fresh sandbox over tree, whose hash it records.
func (a *Activities) rebased(ctx context.Context, d delivery, tree, base string) (gate.Artifact, DeliveryStep, error) {
	if d.pr.RebasedArtifactHash != "" {
		art, _, err := gate.Load(ctx, a.d.Blobs, d.pc.RepoID+"/"+d.pr.RebasedArtifactHash)
		return art, DeliveryStep{}, err
	}
	original, err := gate.Verify(ctx, a.d.Blobs, a.gateResults, d.in.AttemptID, d.pc.RepoID, d.in.ArtifactHash)
	if err != nil {
		return gate.Artifact{}, DeliveryStep{}, err
	}
	_, diff, err := gate.Load(ctx, a.d.Blobs, original.Key)
	if err != nil {
		return gate.Artifact{}, DeliveryStep{}, err
	}
	sb, err := a.newSandbox(ctx, d.cfg, tree, false)
	if err != nil {
		return gate.Artifact{}, DeliveryStep{}, fmt.Errorf("creating the rebase sandbox: %w", err)
	}
	defer func() { _ = sb.Close(context.WithoutCancel(ctx)) }()
	art, err := a.d.Rebase(ctx, sb, a.d.Blobs, original, diff, base)
	if errors.Is(err, gate.ErrRebaseConflict) {
		return gate.Artifact{}, DeliveryStep{Stop: ReasonRebaseConflict, Detail: "the diff does not apply to " + base}, nil
	}
	if err != nil {
		return gate.Artifact{}, DeliveryStep{}, fmt.Errorf("rebasing attempt %s onto %s: %w", d.in.AttemptID, base, err)
	}
	if err := store.SetPullRequestValue(ctx, a.d.DB, d.pr.ID, store.ColRebasedArtifactHash, art.Hash); err != nil {
		return gate.Artifact{}, DeliveryStep{}, err
	}
	return art, DeliveryStep{}, nil
}

// gateRebased gates the rebased artifact in clean_room, over the tree at
// the new base.
func (a *Activities) gateRebased(ctx context.Context, d delivery, tree string, art gate.Artifact) (DeliveryStep, error) {
	cmds := d.cfg.Toolchain.Commands
	out, err := a.d.Gate(ctx, gate.Input{
		AttemptID: d.in.AttemptID, Artifact: art, TreePath: tree, Tier: d.cfg.Access.Tier, Policy: d.cfg.Gates,
		Commands:   gate.Commands{Build: cmds.Build, Lint: cmds.Lint, Test: cmds.Test, Timeout: d.cfg.Sandbox.CommandTimeout},
		NewSandbox: func(ctx context.Context) (contracts.Sandbox, error) { return a.newSandbox(ctx, d.cfg, tree, true) },
	})
	if err != nil {
		return DeliveryStep{}, fmt.Errorf("gating the rebased artifact of attempt %s: %w", d.in.AttemptID, err)
	}
	if !out.Passed {
		return DeliveryStep{Stop: ReasonRebaseGateFailed, Detail: "failed " + strings.Join(failedGates(out.Results), ", ")}, nil
	}
	return DeliveryStep{}, nil
}

// OpenPullRequest pushes the rebased commit to the integration branch,
// over the landed commit, and to the pull request branch, and opens the
// draft pull request from it (FR-S16, FR-S18: drafts until CI passes). It
// returns how to wait for CI, from the repository's configuration.
//
// Idempotent retry: the head commit is deterministic and recorded before
// the pushes; both pushes succeed when the branch already holds it; and
// CreatePullRequest returns the pull request this App already opened
// from the branch, so a retry never opens a second (RL-2). The opened
// pull request is recorded last; a row that records it skips the step.
func (a *Activities) OpenPullRequest(ctx context.Context, in DeliveryInput) (OpenResult, error) {
	res, err := a.openPullRequest(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) openPullRequest(ctx context.Context, in DeliveryInput) (OpenResult, error) {
	d, err := a.loadDelivery(ctx, in, true)
	if err != nil {
		return OpenResult{}, err
	}
	ci := CIWait{PollEvery: d.cfg.Gates.CI.PollEvery, Timeout: d.cfg.Gates.CI.Timeout}
	if d.pr.Number != 0 {
		return OpenResult{Number: d.pr.Number, URL: d.pr.URL, CI: ci}, nil
	}
	if d.pr.RebasedArtifactHash == "" || d.pr.LandedCommit == "" {
		return OpenResult{}, fmt.Errorf("%w: attempt %s is not finalized", errNonRetryable, in.AttemptID)
	}
	scratch, err := os.MkdirTemp(a.d.WorkDir, "open-")
	if err != nil {
		return OpenResult{}, fmt.Errorf("creating scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	head, tree, err := a.buildCommit(ctx, d, scratch, d.pr.RebasedArtifactHash)
	if err != nil {
		return OpenResult{}, err
	}
	if err := store.SetPullRequestValue(ctx, a.d.DB, d.pr.ID, store.ColHeadCommit, head); err != nil {
		return OpenResult{}, err
	}
	if err := a.push(ctx, in.Repo, tree, head, d.pr.IntegrationBranch, d.pr.LandedCommit); err != nil {
		return OpenResult{}, err
	}
	if err := a.push(ctx, in.Repo, tree, head, d.pr.PRBranch, ""); err != nil {
		return OpenResult{}, err
	}
	pr, err := a.openDraft(ctx, d)
	if err != nil {
		return OpenResult{}, err
	}
	if err := store.SetPullRequestOpened(ctx, a.d.DB, d.pr.ID, pr.Number, pr.NodeID, pr.URL); err != nil {
		return OpenResult{}, err
	}
	return OpenResult{Number: pr.Number, URL: pr.URL, CI: ci}, nil
}

// openDraft renders the pull request's body and opens it as a draft.
func (a *Activities) openDraft(ctx context.Context, d delivery) (github.PullRequest, error) {
	results, err := a.gateResults.ForArtifact(ctx, d.in.AttemptID, d.pr.RebasedArtifactHash)
	if err != nil {
		return github.PullRequest{}, fmt.Errorf("reading gate results of attempt %s: %w", d.in.AttemptID, err)
	}
	version, err := readPlanVersion(ctx, a.d.DB, d.pc.PlanID)
	if err != nil {
		return github.PullRequest{}, err
	}
	body := renderPullRequestBody(pullRequestView{
		Issue: d.in.Issue, PlanID: d.pc.PlanID, PlanVersion: version, PR: d.pr, Results: results,
	})
	pr, err := a.d.GitHub.CreatePullRequest(ctx, github.PullRequestSpec{
		Repo: d.in.Repo, Head: d.pr.PRBranch, Base: d.cfg.Sandbox.DefaultBranch,
		Title: d.pr.Title, Body: body, Draft: true,
	})
	if err != nil {
		return github.PullRequest{}, fmt.Errorf("opening the pull request of attempt %s: %w", d.in.AttemptID, err)
	}
	return pr, nil
}

// ReadChecks reads the required checks on the pull request's head commit
// once (FR-A7). Only check runs from the repository's configured CI App
// count, and of several runs of one check the newest does. A read where
// every required check has completed, or one has completed without
// passing, is recorded in pr_ci_results.
//
// Idempotent retry: the read changes nothing on GitHub, and the same runs
// read twice record one row.
func (a *Activities) ReadChecks(ctx context.Context, in DeliveryInput) (ChecksResult, error) {
	res, err := a.readChecks(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) readChecks(ctx context.Context, in DeliveryInput) (ChecksResult, error) {
	d, err := a.loadDelivery(ctx, in, true)
	if err != nil {
		return ChecksResult{}, err
	}
	if d.pr.Number == 0 {
		return ChecksResult{}, fmt.Errorf("%w: attempt %s has no pull request yet", errNonRetryable, in.AttemptID)
	}
	runs, err := a.d.GitHub.ListCheckRuns(ctx, in.Repo, d.pr.HeadCommit)
	if err != nil {
		return ChecksResult{}, fmt.Errorf("reading the checks of %s: %w", d.pr.HeadCommit, err)
	}
	res, checks := evaluateChecks(d.cfg.Gates.CI, runs)
	if !res.Completed {
		return res, nil
	}
	_, err = store.RecordCIResult(ctx, a.d.DB, store.CIResult{
		PullRequestID: d.pr.ID, HeadCommit: d.pr.HeadCommit, Passed: res.Passed, Checks: checks,
	})
	return res, err
}

// MarkReady marks the draft pull request ready for review once its
// latest recorded CI result, on its head commit, passed (FR-A7, FR-S18),
// and records that it did.
//
// Idempotent retry: marking a pull request that is no longer a draft is a
// no-op on GitHub, and the record is set once.
func (a *Activities) MarkReady(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	res, err := a.markReady(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) markReady(ctx context.Context, in DeliveryInput) (DeliveryStep, error) {
	d, err := a.loadDelivery(ctx, in, true)
	if err != nil || d.pr.Ready {
		return DeliveryStep{}, err
	}
	if d.pr.Number == 0 {
		return DeliveryStep{}, fmt.Errorf("%w: attempt %s has no pull request yet", errNonRetryable, in.AttemptID)
	}
	ci, ok, err := store.LatestCIResult(ctx, a.d.DB, d.pr.ID)
	switch {
	case err != nil:
		return DeliveryStep{}, err
	case !ok || !ci.Passed || ci.HeadCommit != d.pr.HeadCommit:
		return DeliveryStep{}, fmt.Errorf("%w: pull request #%d has no passing CI result on %s", errNonRetryable, d.pr.Number, d.pr.HeadCommit)
	}
	if _, err := a.d.GitHub.MarkPullRequestReady(ctx, in.Repo, d.pr.Number); err != nil {
		return DeliveryStep{}, fmt.Errorf("marking pull request #%d ready: %w", d.pr.Number, err)
	}
	return DeliveryStep{}, store.MarkPullRequestReady(ctx, a.d.DB, d.pr.ID)
}

// integrationBranch and prBranch are the issue's two pushed branches
// (FR-S16). Milestone 1 opens one pull request, pr-1.
func integrationBranch(issue int) string { return fmt.Sprintf("agent/%d/integration", issue) }
func prBranch(issue int) string          { return fmt.Sprintf("agent/%d/pr-1", issue) }

// commitMeta is what the commit takes from the attempt and the plan.
type commitMeta struct {
	FinishedAt  time.Time
	Model       string
	PlanVersion int
}
