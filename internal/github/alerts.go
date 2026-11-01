package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"unicode/utf8"
)

// Alert issues (FR-SEC31). These calls reach only the configured alerts
// repository, with its issues-only token (alertTokenPermissions): the
// alert path cannot open or read issues anywhere else, and cannot push.

const (
	// maxTitleRunes is GitHub's limit on an issue title.
	maxTitleRunes = 256
	// issuesPerPage and maxIssuePages bound ListOpenAlertIssues, as
	// commentsPerPage and maxCommentPages bound ListIssueComments.
	issuesPerPage = 100
	maxIssuePages = 10
)

// alertLabel is the shape of a label the alert path sets: lower case,
// digits, colon and hyphen, as in "severity:halt".
var alertLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9:-]{0,49}$`)

// Issue is an alert issue. Body is set by ListOpenAlertIssues.
type Issue struct {
	Number int
	URL    string
	Body   string
}

type issueRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

type issueReply struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    *struct {
		ID int64 `json:"id"`
	} `json:"user"`
	// PullRequest is set on a pull request, which GitHub's issue list
	// also returns.
	PullRequest *struct{} `json:"pull_request"`
}

// CreateAlertIssue opens an issue in the alerts repository with labels,
// assigned to the configured owner login. The body is sanitized first
// (sanitizeMarkdown) and, like the title, checked against the token and
// token shapes.
func (a *App) CreateAlertIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	repo, err := a.alertsRepo()
	if err != nil {
		return Issue{}, err
	}
	if err := checkTitle(title); err != nil {
		return Issue{}, err
	}
	for _, l := range labels {
		if !alertLabel.MatchString(l) {
			return Issue{}, fmt.Errorf("%w: label %q", ErrRefused, l)
		}
	}
	body = sanitizeMarkdown(body)
	tok, err := a.writeToken(ctx, repo, body)
	if err != nil {
		return Issue{}, err
	}
	if err := refuseSecrets(title, tok); err != nil {
		return Issue{}, err
	}
	in := issueRequest{Title: title, Body: body, Labels: labels, Assignees: []string{a.cfg.Alerts.Assignee}}
	var reply issueReply
	if err := a.send(ctx, repo, tok, "creating alert issue", http.MethodPost, repoPath(repo, "/issues"), in, &reply); err != nil {
		return Issue{}, err
	}
	if reply.Number <= 0 {
		return Issue{}, fmt.Errorf("github: creating alert issue: reply has no issue number: %w", ErrInvalidRequest)
	}
	return Issue{Number: reply.Number, URL: reply.HTMLURL}, nil
}

// ListOpenAlertIssues returns the open issues in the alerts repository
// that carry label and that this App's bot opened, oldest first. An issue
// a person opened is left out even if it quotes an alert's marker, so a
// caller deduplicating by marker cannot be steered onto it. As with
// ListIssueComments, a list longer than the page bound is refused rather
// than read in part.
func (a *App) ListOpenAlertIssues(ctx context.Context, label string) ([]Issue, error) {
	repo, err := a.alertsRepo()
	if err != nil {
		return nil, err
	}
	if !alertLabel.MatchString(label) {
		return nil, fmt.Errorf("%w: label %q", ErrRefused, label)
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []Issue
	for page := 1; page <= maxIssuePages; page++ {
		q := url.Values{
			"state": {"open"}, "labels": {label}, "sort": {"created"}, "direction": {"asc"},
			"per_page": {fmt.Sprint(issuesPerPage)}, "page": {fmt.Sprint(page)},
		}
		var batch []issueReply
		if err := a.send(ctx, repo, tok, "listing alert issues", http.MethodGet, repoPath(repo, "/issues?%s", q.Encode()), nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, a.ownIssues(batch)...)
		if len(batch) < issuesPerPage {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github: listing alert issues: more than %d open: %w", maxIssuePages*issuesPerPage, ErrInvalidRequest)
}

// ownIssues keeps the issues this App's bot opened, dropping pull
// requests.
func (a *App) ownIssues(batch []issueReply) []Issue {
	var out []Issue
	for _, r := range batch {
		if r.Number <= 0 || r.PullRequest != nil || r.User == nil || r.User.ID != a.cfg.BotUserID {
			continue
		}
		out = append(out, Issue{Number: r.Number, URL: r.HTMLURL, Body: r.Body})
	}
	return out
}

// CommentOnAlertIssue posts body on an alert issue.
func (a *App) CommentOnAlertIssue(ctx context.Context, issue int, body string) (Comment, error) {
	repo, err := a.alertsRepo()
	if err != nil {
		return Comment{}, err
	}
	return a.CreateComment(ctx, repo, issue, body)
}

// AlertIssueComments returns the comments this App posted on an alert
// issue, oldest first.
func (a *App) AlertIssueComments(ctx context.Context, issue int) ([]Comment, error) {
	repo, err := a.alertsRepo()
	if err != nil {
		return nil, err
	}
	return a.ListIssueComments(ctx, repo, issue)
}

// alertsRepo is the configured alerts repository, refused when the
// configuration has none.
func (a *App) alertsRepo() (RepoRef, error) {
	if err := a.cfg.RequireAlerts(); err != nil {
		return RepoRef{}, fmt.Errorf("%w: no alerts repository: %w", ErrRefused, err)
	}
	return a.cfg.AlertsRepo(), nil
}

func checkTitle(title string) error {
	if title == "" {
		return fmt.Errorf("github: title is empty: %w", ErrInvalidRequest)
	}
	if n := utf8.RuneCountInString(title); n > maxTitleRunes {
		return fmt.Errorf("github: title has %d characters, more than %d: %w", n, maxTitleRunes, ErrInvalidRequest)
	}
	return nil
}
