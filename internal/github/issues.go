package github

import (
	"context"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// maxBodyRunes is GitHub's limit on a comment or pull request body.
// Longer bodies are refused here rather than sent for GitHub to reject.
const maxBodyRunes = 65536

// reactions is every Reaction this package sends.
var reactions = map[Reaction]bool{
	ReactionPlusOne:  true,
	ReactionMinusOne: true,
	ReactionEyes:     true,
	ReactionConfused: true,
	ReactionRocket:   true,
	ReactionHooray:   true,
}

type commentBody struct {
	Body string `json:"body"`
}

type commentReply struct {
	ID                    int64  `json:"id"`
	HTMLURL               string `json:"html_url"`
	PerformedViaGitHubApp *struct {
		ID int64 `json:"id"`
	} `json:"performed_via_github_app"`
}

// CreateComment posts body on an issue or pull request, sanitized first
// (sanitizeMarkdown).
func (a *App) CreateComment(ctx context.Context, repo RepoRef, issue int, body string) (Comment, error) {
	if issue <= 0 {
		return Comment{}, fmt.Errorf("%w: issue number %d", ErrRefused, issue)
	}
	body = sanitizeMarkdown(body)
	tok, err := a.writeToken(ctx, repo, body)
	if err != nil {
		return Comment{}, err
	}
	var reply commentReply
	path := repoPath(repo, "/issues/%d/comments", issue)
	if err := a.send(ctx, repo, tok, "creating comment", http.MethodPost, path, commentBody{Body: body}, &reply); err != nil {
		return Comment{}, err
	}
	if reply.ID <= 0 {
		return Comment{}, fmt.Errorf("github: creating comment: reply has no comment ID: %w", ErrInvalidRequest)
	}
	return Comment{ID: reply.ID, URL: reply.HTMLURL}, nil
}

// EditComment replaces a comment's body in place. It first reads the
// comment and refuses one this App did not post: an installation token can
// edit any comment, and a wrong ID must never rewrite the owner's words.
// The new body is sanitized first (sanitizeMarkdown).
func (a *App) EditComment(ctx context.Context, repo RepoRef, commentID int64, body string) error {
	if commentID <= 0 {
		return fmt.Errorf("%w: comment ID %d", ErrRefused, commentID)
	}
	body = sanitizeMarkdown(body)
	tok, err := a.writeToken(ctx, repo, body)
	if err != nil {
		return err
	}
	path := repoPath(repo, "/issues/comments/%d", commentID)
	var current commentReply
	if err := a.send(ctx, repo, tok, "reading comment", http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	if current.PerformedViaGitHubApp == nil || current.PerformedViaGitHubApp.ID != a.cfg.AppID {
		return fmt.Errorf("%w: comment %d was not posted by this App", ErrRefused, commentID)
	}
	return a.send(ctx, repo, tok, "editing comment", http.MethodPatch, path, commentBody{Body: body}, nil)
}

// AddReaction reacts to an issue or a comment. Reacting twice is not an
// error: GitHub answers an existing reaction with 200.
func (a *App) AddReaction(ctx context.Context, repo RepoRef, t ReactionTarget, r Reaction) error {
	if !reactions[r] {
		return fmt.Errorf("%w: reaction %q is not one this App sends", ErrRefused, r)
	}
	if (t.Issue > 0) == (t.CommentID > 0) || t.Issue < 0 || t.CommentID < 0 {
		return fmt.Errorf("%w: a reaction needs exactly one of an issue or a comment", ErrRefused)
	}
	if err := checkFullName(repo.FullName); err != nil {
		return err
	}
	path := repoPath(repo, "/issues/%d/reactions", t.Issue)
	if t.CommentID > 0 {
		path = repoPath(repo, "/issues/comments/%d/reactions", t.CommentID)
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return err
	}
	in := struct {
		Content Reaction `json:"content"`
	}{Content: r}
	return a.send(ctx, repo, tok, "adding reaction", http.MethodPost, path, in, nil)
}

// writeToken checks a request that publishes body on repo and returns the
// token to send it with. The body is checked against that very token as
// well as against token shapes, so the platform never posts its own
// credential.
func (a *App) writeToken(ctx context.Context, repo RepoRef, body string) (secret, error) {
	if err := checkFullName(repo.FullName); err != nil {
		return secret{}, err
	}
	if err := checkBody(body); err != nil {
		return secret{}, err
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return secret{}, err
	}
	if err := refuseSecrets(body, tok); err != nil {
		return secret{}, err
	}
	return tok, nil
}

func checkBody(body string) error {
	if body == "" {
		return fmt.Errorf("github: body is empty: %w", ErrInvalidRequest)
	}
	if n := utf8.RuneCountInString(body); n > maxBodyRunes {
		return fmt.Errorf("github: body has %d characters, more than %d: %w", n, maxBodyRunes, ErrInvalidRequest)
	}
	return nil
}

// commentsPerPage is the page size ListIssueComments asks for, GitHub's
// maximum, and maxCommentPages bounds how many pages it reads, so an issue
// with an enormous thread cannot hold an activity forever.
const (
	commentsPerPage = 100
	maxCommentPages = 20
)

// listedComment is one entry of GitHub's issue comment list.
type listedComment struct {
	commentReply
	Body string `json:"body"`
}

// ListIssueComments reads an issue's comments page by page and returns
// those performed via this App, oldest first. A thread longer than
// maxCommentPages pages is refused rather than read in part: a partial
// read could miss the comment the caller is looking for, and the caller
// would then post a duplicate.
func (a *App) ListIssueComments(ctx context.Context, repo RepoRef, issue int) ([]Comment, error) {
	if issue <= 0 {
		return nil, fmt.Errorf("%w: issue number %d", ErrRefused, issue)
	}
	if err := checkFullName(repo.FullName); err != nil {
		return nil, err
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []Comment
	for page := 1; page <= maxCommentPages; page++ {
		var batch []listedComment
		path := repoPath(repo, "/issues/%d/comments?per_page=%d&page=%d", issue, commentsPerPage, page)
		if err := a.send(ctx, repo, tok, "listing comments", http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, a.ownComments(batch)...)
		if len(batch) < commentsPerPage {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github: listing comments: issue %d has more than %d comments: %w",
		issue, maxCommentPages*commentsPerPage, ErrInvalidRequest)
}

// ownComments keeps the comments performed via this App.
func (a *App) ownComments(batch []listedComment) []Comment {
	var out []Comment
	for _, c := range batch {
		if c.ID <= 0 || c.PerformedViaGitHubApp == nil || c.PerformedViaGitHubApp.ID != a.cfg.AppID {
			continue
		}
		out = append(out, Comment{ID: c.ID, URL: c.HTMLURL, Body: c.Body})
	}
	return out
}
