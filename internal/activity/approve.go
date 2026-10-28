package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/store"
)

// Approve pins the base commit (FR-S19) and starts execution: it reads
// the default branch's head and, in one transaction, approves the plan
// with that SHA and moves the feature request from awaiting_approval to
// executing. Either both land or neither does.
//
// Idempotent retry: the outside action is a read-only clone, which leaves
// nothing behind on GitHub. The plan is read first, and a plan already
// approved returns the SHA pinned then, without cloning, so a retry after
// a commit whose result was lost cannot pin a newer commit. Both changes
// are compare-and-set, and ErrAlreadyApplied counts as success.
func (a *Activities) Approve(ctx context.Context, in ApproveInput) (ApproveResult, error) {
	res, err := a.approve(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) approve(ctx context.Context, in ApproveInput) (ApproveResult, error) {
	plan, err := readPlan(ctx, a.d.DB, in.PlanID)
	if err != nil {
		return ApproveResult{}, err
	}
	if plan.FeatureRequestID != in.FeatureRequestID {
		return ApproveResult{}, fmt.Errorf("%w: plan %s belongs to feature request %s, not %s",
			errNonRetryable, in.PlanID, plan.FeatureRequestID, in.FeatureRequestID)
	}
	sha := plan.BaseSHA
	if plan.Status != contracts.PlanApproved {
		if sha, err = a.defaultHead(ctx, in); err != nil {
			return ApproveResult{}, err
		}
	}
	err = pgx.BeginFunc(ctx, a.d.DB, func(tx pgx.Tx) error {
		if err := applied(store.ApprovePlan(ctx, tx, in.PlanID, in.PlanRevision, sha, in.Actor)); err != nil {
			return err
		}
		return applied(store.Transition(ctx, tx, store.TransitionRequest{
			Kind: contracts.EntityFeatureRequest, ID: in.FeatureRequestID, Revision: in.FeatureRevision,
			From: string(contracts.FeatureAwaitingApproval), To: string(contracts.FeatureExecuting), Actor: in.Actor,
		}))
	})
	if err != nil {
		return ApproveResult{}, fmt.Errorf("approving plan %s: %w", in.PlanID, err)
	}
	// Read back what is pinned: if an earlier call committed between the
	// read above and this transaction, its SHA is the one that stands.
	if plan, err = readPlan(ctx, a.d.DB, in.PlanID); err != nil {
		return ApproveResult{}, err
	}
	return ApproveResult{BaseSHA: plan.BaseSHA, FeatureRevision: in.FeatureRevision + 1, PlanRevision: in.PlanRevision + 1}, nil
}

// defaultHead clones the repository's configured default branch into a
// scratch directory and returns its head commit.
func (a *Activities) defaultHead(ctx context.Context, in ApproveInput) (string, error) {
	cfg, err := a.repo(in.Repo.FullName)
	if err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp(a.d.WorkDir, "approve-")
	if err != nil {
		return "", fmt.Errorf("creating scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	dir := filepath.Join(scratch, "tree")
	if err := a.d.GitHub.Clone(ctx, in.Repo, dir, cfg.Sandbox.DefaultBranch); err != nil {
		return "", fmt.Errorf("cloning %s at %s to pin the base commit: %w", in.Repo.FullName, cfg.Sandbox.DefaultBranch, err)
	}
	sha, err := sandbox.HeadCommit(dir)
	if err != nil {
		return "", fmt.Errorf("reading the head of %s: %w", cfg.Sandbox.DefaultBranch, err)
	}
	if sha == "" {
		return "", errors.New("default branch has no head commit")
	}
	return sha, nil
}
