package github

import (
	"context"
	"errors"
)

// CreateComment posts body on an issue or pull request.
//
// L0 stub: W2 implements.
func (a *App) CreateComment(ctx context.Context, repo RepoRef, issue int, body string) (Comment, error) {
	_, _, _, _ = ctx, repo, issue, body
	return Comment{}, errors.New("github: CreateComment not implemented")
}

// EditComment replaces a comment's body in place. It first reads the
// comment and refuses one this App did not post: an installation token can
// edit any comment, and a wrong ID must never rewrite the owner's words.
//
// L0 stub: W2 implements.
func (a *App) EditComment(ctx context.Context, repo RepoRef, commentID int64, body string) error {
	_, _, _, _ = ctx, repo, commentID, body
	return errors.New("github: EditComment not implemented")
}

// AddReaction reacts to an issue or a comment. Reacting twice is not an
// error: GitHub answers an existing reaction with 200.
//
// L0 stub: W2 implements.
func (a *App) AddReaction(ctx context.Context, repo RepoRef, t ReactionTarget, r Reaction) error {
	_, _, _, _ = ctx, repo, t, r
	return errors.New("github: AddReaction not implemented")
}
