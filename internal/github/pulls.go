package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// baseBranch is the shape of branch a pull request may target. It admits
// the names a repository's default and release branches use, and nothing
// that reads as an option or climbs out of refs/heads.
var baseBranch = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

type pullReply struct {
	Number  int    `json:"number"`
	NodeID  string `json:"node_id"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	User    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type pullCreate struct {
	Title               string `json:"title"`
	Head                string `json:"head"`
	Base                string `json:"base"`
	Body                string `json:"body"`
	Draft               bool   `json:"draft"`
	MaintainerCanModify bool   `json:"maintainer_can_modify"`
}

// CreatePullRequest opens a pull request from an agent/ branch. It first
// looks for an open one from the same head and returns it if this App
// opened it, so a retried activity never opens a second (RL-2). One opened
// by anyone else is refused, not adopted. A 422 from a concurrent create
// looks again. The title and body are sanitized first (sanitizeMarkdown).
func (a *App) CreatePullRequest(ctx context.Context, p PullRequestSpec) (PullRequest, error) {
	p.Title, p.Body = sanitizeMarkdown(p.Title), sanitizeMarkdown(p.Body)
	if err := checkPullRequest(p); err != nil {
		return PullRequest{}, err
	}
	tok, err := a.token(ctx, p.Repo)
	if err != nil {
		return PullRequest{}, err
	}
	if err := refuseSecrets(p.Title+"\n"+p.Body, tok); err != nil {
		return PullRequest{}, err
	}
	if pr, found, err := a.findPullRequest(ctx, p, tok); err != nil || found {
		return pr, err
	}
	pr, createErr := a.openPullRequest(ctx, p, tok)
	var apiErr *APIError
	if !errors.As(createErr, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		return pr, createErr
	}
	// GitHub answers a create for a head that already has an open pull
	// request with 422; another attempt may have opened it in between.
	pr, found, err := a.findPullRequest(ctx, p, tok)
	if err != nil {
		return PullRequest{}, err
	}
	if !found {
		return PullRequest{}, createErr
	}
	return pr, nil
}

func checkPullRequest(p PullRequestSpec) error {
	if err := checkFullName(p.Repo.FullName); err != nil {
		return err
	}
	if err := CheckBranch(p.Head); err != nil {
		return err
	}
	if !baseBranch.MatchString(p.Base) || strings.HasPrefix(p.Base, "-") || strings.Contains(p.Base, "..") {
		return fmt.Errorf("%w: base branch %q", ErrRefused, p.Base)
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("github: pull request title is empty: %w", ErrInvalidRequest)
	}
	if p.Body != "" {
		return checkBody(p.Body)
	}
	return nil
}

// findPullRequest returns the open pull request from p.Head to p.Base, if
// there is one. One whose author is not this App's bot is refused: adopting
// it would let whoever opened it choose what the platform reports as its
// work.
func (a *App) findPullRequest(ctx context.Context, p PullRequestSpec, tok secret) (PullRequest, bool, error) {
	owner, _, _ := strings.Cut(p.Repo.FullName, "/")
	q := url.Values{"head": {owner + ":" + p.Head}, "state": {"open"}, "per_page": {"100"}}
	var open []pullReply
	path := repoPath(p.Repo, "/pulls?%s", q.Encode())
	if err := a.send(ctx, p.Repo, tok, "listing pull requests", http.MethodGet, path, nil, &open); err != nil {
		return PullRequest{}, false, err
	}
	for _, pr := range open {
		// GitHub ignores a head filter it cannot parse and lists every
		// open pull request, so the head is checked here too.
		if pr.Head.Ref != p.Head {
			continue
		}
		if pr.User.Type != "Bot" || pr.User.Login != a.cfg.AppSlug+"[bot]" {
			return PullRequest{}, false, fmt.Errorf("%w: open pull request #%d from %s was not opened by this App",
				ErrRefused, pr.Number, p.Head)
		}
		if pr.Base.Ref != p.Base {
			return PullRequest{}, false, fmt.Errorf("%w: open pull request #%d from %s targets another base",
				ErrRefused, pr.Number, p.Head)
		}
		return pullRequest(pr, false), true, nil
	}
	return PullRequest{}, false, nil
}

func (a *App) openPullRequest(ctx context.Context, p PullRequestSpec, tok secret) (PullRequest, error) {
	in := pullCreate{Title: p.Title, Head: p.Head, Base: p.Base, Body: p.Body, Draft: p.Draft}
	var reply pullReply
	if err := a.send(ctx, p.Repo, tok, "creating pull request", http.MethodPost, repoPath(p.Repo, "/pulls"), in, &reply); err != nil {
		return PullRequest{}, err
	}
	if reply.Number <= 0 {
		return PullRequest{}, fmt.Errorf("github: creating pull request: reply has no number: %w", ErrInvalidRequest)
	}
	return pullRequest(reply, true), nil
}

func pullRequest(r pullReply, created bool) PullRequest {
	return PullRequest{Number: r.Number, NodeID: r.NodeID, URL: r.HTMLURL, Draft: r.Draft, Created: created}
}

// markReadyMutation is the only GraphQL the client sends. The REST API
// cannot take a pull request out of draft.
const markReadyMutation = `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type markReadyReply struct {
	Data struct {
		Mark struct {
			PullRequest struct {
				IsDraft bool `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"markPullRequestReadyForReview"`
	} `json:"data"`
	// Errors are counted, never read: GitHub's messages quote the request.
	Errors []struct{} `json:"errors"`
}

// MarkPullRequestReady takes pull request number out of draft. It reads
// the pull request first and refuses one this App's bot did not open, or
// whose head is not an agent/ branch: the platform marks ready only its
// own work. One that is already not a draft is returned as it is, so a
// retry after a mark that succeeded unreported sends no second mutation
// (RL-2).
func (a *App) MarkPullRequestReady(ctx context.Context, repo RepoRef, number int) (PullRequest, error) {
	if err := checkFullName(repo.FullName); err != nil {
		return PullRequest{}, err
	}
	if number <= 0 {
		return PullRequest{}, fmt.Errorf("%w: pull request number %d", ErrRefused, number)
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return PullRequest{}, fmt.Errorf("github: marking pull request #%d ready: %w", number, err)
	}
	var reply pullReply
	if err := a.send(ctx, repo, tok, "reading pull request", http.MethodGet, repoPath(repo, "/pulls/%d", number), nil, &reply); err != nil {
		return PullRequest{}, err
	}
	if err := a.checkOwnPull(reply, number); err != nil {
		return PullRequest{}, err
	}
	pr := pullRequest(reply, false)
	if !pr.Draft {
		return pr, nil
	}
	in := graphQLRequest{Query: markReadyMutation, Variables: map[string]any{"id": reply.NodeID}}
	var out markReadyReply
	if err := a.send(ctx, repo, tok, "marking pull request ready", http.MethodPost, "/graphql", in, &out); err != nil {
		return PullRequest{}, err
	}
	if len(out.Errors) > 0 || out.Data.Mark.PullRequest.IsDraft {
		return PullRequest{}, fmt.Errorf("github: marking pull request #%d ready: GraphQL refused the mutation: %w", number, ErrInvalidRequest)
	}
	pr.Draft = false
	return pr, nil
}

// checkOwnPull refuses a pull request this App's bot did not open from an
// agent/ branch.
func (a *App) checkOwnPull(r pullReply, number int) error {
	switch {
	case r.Number != number || r.NodeID == "":
		return fmt.Errorf("github: reading pull request #%d: reply names another pull request: %w", number, ErrInvalidRequest)
	case r.User.Type != "Bot" || r.User.Login != a.cfg.AppSlug+"[bot]":
		return fmt.Errorf("%w: pull request #%d was not opened by this App", ErrRefused, number)
	}
	if err := CheckBranch(r.Head.Ref); err != nil {
		return fmt.Errorf("pull request #%d: %w", number, err)
	}
	return nil
}
