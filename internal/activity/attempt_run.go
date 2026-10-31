package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// attemptRun is one RunAttempt in progress.
type attemptRun struct {
	a    *Activities
	in   RunAttemptInput
	row  attemptRow
	pc   planContext
	body PlanBody
	spec contracts.RoleSpec
	cfg  Repo
	info activity.Info
	log  log.Logger

	scratch string
	tree    string
	sb      contracts.Sandbox

	out    contracts.SessionOutput
	report *contracts.CompletionReport
}

// execute runs the attempt's steps in order. Every path that leaves it
// after the sandbox exists releases the sandbox.
func (r *attemptRun) execute(ctx context.Context) (AttemptOutcome, error) {
	if err := r.prepareTree(ctx); err != nil {
		return r.errored(ctx, contracts.ReasonSandboxError, err)
	}
	if err := r.startSandbox(ctx); err != nil {
		return r.errored(ctx, contracts.ReasonSandboxError, err)
	}
	defer r.release(ctx, true)
	if out, done, err := r.runAgent(ctx); done {
		return out, err
	}
	return r.gate(ctx)
}

// prepareTree has the tree source prepare the plan's repository at the
// pinned base commit in the worker's scratch space, and checks that its
// head is that commit.
func (r *attemptRun) prepareTree(ctx context.Context) error {
	scratch, err := os.MkdirTemp(r.a.d.WorkDir, "attempt-")
	if err != nil {
		return fmt.Errorf("creating scratch directory: %w", err)
	}
	r.scratch, r.tree = scratch, filepath.Join(scratch, "tree")
	if r.body.Repository.FullName != r.in.RepoFullName {
		return fmt.Errorf("%w: plan %s names repository %q, not %s", errNonRetryable, r.pc.PlanID, r.body.Repository.FullName, r.in.RepoFullName)
	}
	head, err := r.a.d.Trees.Prepare(ctx, TreeRequest{Repository: r.body.Repository, Ref: r.in.BaseSHA, Dir: r.tree})
	if err != nil {
		return fmt.Errorf("preparing the tree for attempt %s: %w", r.in.AttemptID, err)
	}
	if head != r.in.BaseSHA {
		return fmt.Errorf("%w: clone is at %s, not the pinned %s", errNonRetryable, head, r.in.BaseSHA)
	}
	return nil
}

// startSandbox creates the sandbox and records its ID on the attempt as
// the first write after Create, before any agent step, so cleanup can
// name it after a crash (RL-3). A sandbox whose ID cannot be recorded is
// released at once and is not marked released: the mark would then stand
// for a sandbox the row does not name, and could hide one it does.
func (r *attemptRun) startSandbox(ctx context.Context) error {
	sb, err := r.create(ctx)
	if err != nil {
		return fmt.Errorf("creating sandbox: %w", err)
	}
	r.sb = sb
	if err := store.SetAttemptSandbox(ctx, r.a.d.DB, r.in.AttemptID, sb.ID()); err != nil {
		r.release(ctx, false)
		r.sb = nil
		return fmt.Errorf("recording sandbox %s on attempt %s: %w", sb.ID(), r.in.AttemptID, err)
	}
	return nil
}

// create makes a sandbox over the clone, with the repository's install
// phase when it configures one. The gate's clean-room sandbox is made the
// same way.
func (r *attemptRun) create(ctx context.Context) (contracts.Sandbox, error) {
	return r.a.newSandbox(ctx, r.cfg, r.tree, true)
}

// release removes the attempt's sandbox and, when record is set, marks it
// released. It runs on a context detached from ctx, so a cancelled
// attempt still cleans up. A failure is logged: the workflow's
// ReleaseAttemptSandbox and the startup sweep retry it from the recorded
// ID.
func (r *attemptRun) release(ctx context.Context, record bool) {
	if r.sb == nil {
		return
	}
	ctx, cancel := detached(ctx, recordTimeout)
	defer cancel()
	id := r.sb.ID()
	err := r.a.d.Sandboxes.Release(ctx, id)
	if err == nil && record {
		err = store.MarkSandboxReleased(ctx, r.a.d.DB, r.in.AttemptID)
	}
	if err != nil {
		r.log.Error("releasing attempt sandbox; the workflow and the sweep will retry", "attempt_id", r.in.AttemptID, "sandbox_id", id, "error", err)
	}
}

// runAgent runs the agent session. done reports that the attempt ended
// here, recorded, and out and err are its result.
func (r *attemptRun) runAgent(ctx context.Context) (out AttemptOutcome, done bool, err error) {
	runner, err := r.runner(ctx)
	if err != nil {
		out, err = r.errored(ctx, contracts.ReasonProviderError, err)
		return out, true, err
	}
	session, err := runner.Run(ctx, r.sb, r.sessionInput())
	r.out = session
	switch {
	case err == nil && session.Reason == "":
		r.report = &r.out.Report
		return AttemptOutcome{}, false, nil
	case err == nil:
		out, err = r.finish(ctx, contracts.AttemptFailed, session.Reason)
		return out, true, err
	}
	out, err = r.sessionError(ctx, session.Reason, err)
	return out, true, err
}

// runner builds the attempt's runner over a router scope carrying the
// attempt and feature request IDs (FR-SEC9); the request's prompt version
// comes from the role spec inside the runner.
func (r *attemptRun) runner(ctx context.Context) (*agent.Runner, error) {
	key := fmt.Sprintf("%s/%s/%d", r.info.WorkflowExecution.ID, r.info.ActivityID, r.info.Attempt)
	scope, err := r.a.d.Router.Scope(ctx, llm.Scope{
		RepoID: r.pc.RepoID, FeatureRequestID: r.pc.FeatureRequestID, AttemptID: r.in.AttemptID,
		KeyPrefix: key, TokenBudget: r.spec.Limits.MaxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("scoping the router to attempt %s: %w", r.in.AttemptID, err)
	}
	return agent.New(agent.Config{
		Router: scope, Recorder: r.a.toolCalls, Blobs: r.a.d.Blobs,
		AttemptID: r.in.AttemptID, SessionKey: key,
		Commands: r.cfg.Toolchain.Commands, OutputCap: AgentOutputCap, MaxCommandTimeout: r.cfg.Sandbox.CommandTimeout,
	})
}

// sessionInput is the frozen plan body, as approved.
func (r *attemptRun) sessionInput() contracts.SessionInput {
	return contracts.SessionInput{
		RepoID: r.pc.RepoID, Spec: r.spec, Profile: r.body.Profile, Task: r.body.Task,
		Limits: r.body.Constraints, IssueText: r.body.IssueText, BaseCommit: r.in.BaseSHA,
	}
}

// sessionError records a session that ended in an error (runner.go's
// contract): a cap refusal is errored budget_cap and returns the cap's
// typed error; a sandbox or provider failure carries its reason; anything
// else (an audit or blob write that failed) is the platform's, a sandbox
// error.
func (r *attemptRun) sessionError(ctx context.Context, reason contracts.FailureReason, err error) (AttemptOutcome, error) {
	var budget llm.BudgetError
	if !cancelled(ctx) && errors.As(err, &budget) {
		out, ferr := r.finish(ctx, contracts.AttemptErrored, contracts.ReasonBudgetCap)
		if ferr != nil {
			return out, ferr
		}
		return out, err
	}
	if reason == "" {
		reason = contracts.ReasonSandboxError
	}
	return r.errored(ctx, reason, err)
}

// gate freezes the artifact, records its hash, gates it, and records the
// verdict. An artifact over the size cap is the attempt's own failure; a
// gating environment failure is errored, with nothing recorded by the
// gate (FR-SEC27).
func (r *attemptRun) gate(ctx context.Context) (AttemptOutcome, error) {
	art, err := gate.Freeze(ctx, r.sb, r.a.d.Blobs, r.pc.RepoID, r.in.BaseSHA)
	if errors.Is(err, gate.ErrArtifactTooLarge) {
		return r.finish(ctx, contracts.AttemptFailed, contracts.ReasonGateFailed)
	}
	if err != nil {
		return r.errored(ctx, contracts.ReasonSandboxError, err)
	}
	if err := store.SetArtifactHash(ctx, r.a.d.DB, r.in.AttemptID, art.Hash); err != nil {
		return r.errored(ctx, contracts.ReasonSandboxError, err)
	}
	verdict, err := r.a.d.Gate(ctx, gate.Input{
		AttemptID: r.in.AttemptID, Artifact: art, TreePath: r.tree,
		Tier: r.cfg.Access.Tier, Policy: r.cfg.Gates, InPlace: r.sb, NewSandbox: r.create,
		Commands: gate.Commands{
			Build: r.cfg.Toolchain.Commands.Build, Lint: r.cfg.Toolchain.Commands.Lint,
			Test: r.cfg.Toolchain.Commands.Test, Timeout: r.cfg.Sandbox.CommandTimeout,
		},
	})
	if err != nil {
		return r.errored(ctx, contracts.ReasonSandboxError, err)
	}
	if !verdict.Passed {
		return r.finish(ctx, contracts.AttemptFailed, contracts.ReasonGateFailed)
	}
	return r.finish(ctx, contracts.AttemptPassed, "")
}

// errored records the attempt errored. A cancelled context (not one past
// its deadline, which is a timeout, not the owner) overrides the reason
// with cancelled and returns the context's error; a permanent
// cause returns as permanent, so the workflow starts no new ordinal for
// it; any other cause returns the recorded outcome as a value.
func (r *attemptRun) errored(ctx context.Context, reason contracts.FailureReason, cause error) (AttemptOutcome, error) {
	if cancelled(ctx) {
		reason = contracts.ReasonCancelled
	}
	r.log.Warn("attempt errored", "attempt_id", r.in.AttemptID, "reason", string(reason), "error", cause)
	out, err := r.finish(ctx, contracts.AttemptErrored, reason)
	switch {
	case err != nil:
		return out, err
	case cancelled(ctx):
		return out, ctx.Err()
	case IsPermanent(cause):
		return out, cause
	}
	return out, nil
}

// finish moves the attempt to its terminal state with its usage, actual
// model and provider, and report, on a context detached from ctx so a
// cancelled attempt is still recorded. A finish that already landed is
// success. It returns the outcome as recorded.
func (r *attemptRun) finish(ctx context.Context, to contracts.AttemptState, reason contracts.FailureReason) (AttemptOutcome, error) {
	ctx, cancel := detached(ctx, recordTimeout)
	defer cancel()
	err := pgx.BeginFunc(ctx, r.a.d.DB, func(tx pgx.Tx) error {
		return applied(store.FinishAttempt(ctx, tx, store.FinishParams{
			AttemptID: r.in.AttemptID, Revision: r.row.Revision, To: to, Reason: reason,
			Actor: store.ActorWorkflow(r.info.WorkflowExecution.ID),
			Usage: r.out.Usage, Model: r.out.Provenance.Model, Provider: r.out.Provenance.Provider,
			Report: r.report,
		}))
	})
	var wrong *store.WrongStateError
	if err != nil && !errors.As(err, &wrong) {
		return AttemptOutcome{}, fmt.Errorf("recording the outcome of attempt %s: %w", r.in.AttemptID, err)
	}
	row, err := readAttempt(ctx, r.a.d.DB, r.in.AttemptID)
	if err != nil {
		return AttemptOutcome{}, err
	}
	return row.outcome(), nil
}

// cancelled reports whether ctx was cancelled, as Temporal cancels an
// activity whose workflow asked for it, rather than timed out.
func cancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}
