package github

import (
	"context"
	"errors"
)

// CreatePullRequest opens a pull request from an agent/ branch. It first
// looks for an open one from the same head and returns it if this App
// opened it, so a retried activity never opens a second (RL-2). One opened
// by anyone else is refused, not adopted. A 422 from a concurrent create
// looks again.
//
// L0 stub: W2 implements.
func (a *App) CreatePullRequest(ctx context.Context, p PullRequestSpec) (PullRequest, error) {
	_, _ = ctx, p
	return PullRequest{}, errors.New("github: CreatePullRequest not implemented")
}
