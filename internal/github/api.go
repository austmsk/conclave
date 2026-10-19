package github

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultTimeout bounds one GitHub API request when NewApp is given no
// client.
const DefaultTimeout = 30 * time.Second

// minKeyBits is the smallest App key accepted. GitHub issues 2048-bit keys.
const minKeyBits = 2048

// Client is every repository write the platform makes. It is the boundary
// ticket 8's activities depend on, and the single choke point for writes
// to GitHub (NFR-M5): nothing else in Conclave holds a token.
type Client interface {
	// Clone clones repo at ref into dir, which must not exist, and
	// prepares the tree for a sandbox.
	Clone(ctx context.Context, repo RepoRef, dir, ref string) error
	// Push pushes one commit to one agent/ branch, with a lease.
	Push(ctx context.Context, p PushSpec) error
	// CreatePullRequest opens a pull request from an agent/ branch, or
	// returns the one this App already opened from it.
	CreatePullRequest(ctx context.Context, p PullRequestSpec) (PullRequest, error)
	// CreateComment posts a comment on an issue or pull request.
	CreateComment(ctx context.Context, repo RepoRef, issue int, body string) (Comment, error)
	// EditComment replaces the body of a comment this App posted.
	EditComment(ctx context.Context, repo RepoRef, commentID int64, body string) error
	// AddReaction reacts to an issue or a comment.
	AddReaction(ctx context.Context, repo RepoRef, t ReactionTarget, r Reaction) error
}

// PullRequestSpec describes a pull request to open.
type PullRequestSpec struct {
	Repo RepoRef
	// Head must be an agent/ branch (CheckBranch).
	Head  string
	Base  string
	Title string
	Body  string
	Draft bool
}

// PullRequest is an opened pull request.
type PullRequest struct {
	Number int
	NodeID string
	URL    string
	Draft  bool
	// Created is false when an existing pull request was returned.
	Created bool
}

// Comment is a posted comment.
type Comment struct {
	ID  int64
	URL string
}

// ReactionTarget is an issue or a comment; exactly one field is set.
type ReactionTarget struct {
	Issue     int
	CommentID int64
}

// Reaction is a reaction content. Only these are sent.
type Reaction string

const (
	ReactionPlusOne  Reaction = "+1"
	ReactionMinusOne Reaction = "-1"
	ReactionEyes     Reaction = "eyes"
	ReactionConfused Reaction = "confused"
	ReactionRocket   Reaction = "rocket"
	ReactionHooray   Reaction = "hooray"
)

var (
	// ErrNotFound reports a 404: the repository, issue, or comment does not
	// exist or the installation cannot see it.
	ErrNotFound = errors.New("github: not found")
	// ErrAuth reports a rejected credential: a bad JWT, a revoked
	// installation, an expired token.
	ErrAuth = errors.New("github: credentials rejected")
	// ErrRateLimited reports a primary or secondary rate limit.
	ErrRateLimited = errors.New("github: rate limited")
	// ErrUnavailable reports a transport failure or a 5xx.
	ErrUnavailable = errors.New("github: unavailable")
	// ErrInvalidRequest reports a 4xx GitHub will answer the same way
	// again, or a reply this client cannot use.
	ErrInvalidRequest = errors.New("github: invalid request")
)

// APIError is a failed GitHub API call. Its text is fixed: the operation,
// the status, and GitHub's request ID. GitHub's message is dropped, because
// validation messages quote the request, and this error travels into
// Temporal history and logs. Unwrap returns only the sentinel, never the
// response.
type APIError struct {
	Op         string
	Status     int
	RequestID  string
	RetryAfter time.Duration
	kind       error
}

func (e *APIError) Error() string {
	if e.RequestID == "" {
		return fmt.Sprintf("github: %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("github: %s: HTTP %d (request %s)", e.Op, e.Status, e.RequestID)
}

func (e *APIError) Unwrap() error { return e.kind }

// App is the Client, authenticated as the GitHub App. It holds the private
// key and caches installation tokens, so only the worker constructs one;
// the internet-facing server never does.
type App struct {
	cfg  Config
	key  *rsa.PrivateKey
	http *http.Client
	now  func() time.Time
	// token returns an installation token for repo. It is
	// installationToken outside tests.
	token func(ctx context.Context, repo RepoRef) (secret, error)

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

var _ Client = (*App)(nil)

// NewApp returns the App client. hc may be nil for a client with
// DefaultTimeout; one given must have a positive timeout. NewApp keeps a
// copy that never follows a redirect: a redirected request would carry its
// token to wherever Location points.
func NewApp(cfg Config, key *rsa.PrivateKey, hc *http.Client) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if key == nil || key.N.BitLen() < minKeyBits {
		return nil, fmt.Errorf("%w: App key must be RSA of at least %d bits", ErrInvalidConfig, minKeyBits)
	}
	client := http.Client{Timeout: DefaultTimeout}
	if hc != nil {
		if hc.Timeout <= 0 {
			return nil, fmt.Errorf("%w: HTTP client needs a timeout", ErrInvalidConfig)
		}
		client = *hc
	}
	client.CheckRedirect = refuseRedirect
	a := &App{cfg: cfg, key: key, http: &client, now: time.Now, tokens: map[int64]cachedToken{}}
	a.token = a.installationToken
	return a, nil
}

// Format prints an App without its key or tokens. fmt reaches unexported
// fields by reflection, without calling their methods, so the secret
// type's own redaction would not apply to a %+v of an App.
func (a *App) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "github.App{app_id: %d, %s}", a.cfg.AppID, redacted)
}

// refuseRedirect stops at the first redirect and hands back the 3xx
// response, which becomes an APIError.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// do sends one API request authenticated with tok, encoding in as JSON
// when it is non-nil and decoding a 2xx reply into out when it is
// non-nil. Every failure is an *APIError or a context error.
//
// L0 stub: W2 implements.
func (a *App) do(ctx context.Context, op, method, path string, tok secret, in, out any) error {
	_, _, _, _, _, _ = ctx, method, path, tok, in, out
	return &APIError{Op: op, kind: ErrUnavailable}
}
