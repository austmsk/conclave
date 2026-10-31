package activity

import (
	"context"
	"fmt"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/sandbox"
)

// TreeSource prepares the working tree an attempt runs on: the
// repository at a commit, with no remotes and no credentials. It is the
// boundary between a task and where its code comes from. Milestone 1
// clones through the GitHub App (GitHubTrees); Milestone 2's benchmark
// substitutes its own, so a task needs no GitHub to run.
type TreeSource interface {
	// Prepare creates req.Dir, which must not exist, holding the tree at
	// req.Ref, and returns the commit its HEAD names.
	Prepare(ctx context.Context, req TreeRequest) (head string, err error)
}

// TreeRequest is one tree to prepare.
type TreeRequest struct {
	Repository PlanRepository
	Ref        string
	Dir        string
}

// GitHubTrees is Milestone 1's TreeSource: a clone through the App's one
// installation, whose token is scoped to the repository the plan names
// by numeric ID. github.App.Clone leaves no remotes or credentials.
type GitHubTrees struct {
	Client         github.Client
	InstallationID int64
}

var _ TreeSource = GitHubTrees{}

// Prepare clones req.Repository at req.Ref into req.Dir and returns its
// HEAD.
func (g GitHubTrees) Prepare(ctx context.Context, req TreeRequest) (string, error) {
	if req.Repository.GitHubID <= 0 {
		return "", fmt.Errorf("%w: plan names no GitHub repository ID for %s", errNonRetryable, req.Repository.FullName)
	}
	repo := github.RepoRef{ID: req.Repository.GitHubID, FullName: req.Repository.FullName, InstallationID: g.InstallationID}
	if err := g.Client.Clone(ctx, repo, req.Dir, req.Ref); err != nil {
		return "", fmt.Errorf("cloning %s at %s: %w", req.Repository.FullName, req.Ref, err)
	}
	head, err := sandbox.HeadCommit(req.Dir)
	if err != nil {
		return "", fmt.Errorf("reading the head of the clone of %s: %w", req.Repository.FullName, err)
	}
	return head, nil
}
