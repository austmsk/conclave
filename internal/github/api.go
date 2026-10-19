package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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

const (
	// apiVersion pins the REST API version every request asks for.
	apiVersion = "2022-11-28"
	// maxErrorBody bounds how much of an error reply is read before it is
	// discarded; maxReplyBody bounds a reply that is decoded.
	maxErrorBody = 4 << 10
	maxReplyBody = 1 << 20
)

// requestIDPattern is the shape of GitHub's X-GitHub-Request-Id. Anything
// else in that header is dropped, so a server cannot put text of its
// choosing, a token included, into an error through it.
var requestIDPattern = regexp.MustCompile(`^[A-Fa-f0-9:]{1,64}$`)

// do sends one API request authenticated with tok, encoding in as JSON
// when it is non-nil and decoding a 2xx reply into out when it is
// non-nil. Every failure is an *APIError or a context error. path is built
// by the caller from validated parts and begins with a slash.
func (a *App) do(ctx context.Context, op, method, path string, tok secret, in, out any) error {
	req, err := a.newRequest(ctx, method, path, tok, in)
	if err != nil {
		// The cause could quote the request; it is dropped.
		return &APIError{Op: op, kind: ErrInvalidRequest}
	}
	resp, err := a.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("github: %s: %w", op, ctx.Err())
		}
		// A transport error names the URL but never the headers, so the
		// token cannot leak through it; the cause is still dropped to be
		// sure.
		return &APIError{Op: op, kind: ErrUnavailable}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(op, resp)
	}
	body := io.LimitReader(resp.Body, maxReplyBody)
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return decodeError(ctx, op, resp, err)
	}
	return nil
}

func (a *App) newRequest(ctx context.Context, method, path string, tok secret, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIBaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "conclave")
	req.Header.Set("Authorization", "Bearer "+tok.reveal())
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// decodeError reports a 2xx reply that could not be read. A reply cut off
// by the context or the network is worth asking for again; one that does
// not decode is not.
func decodeError(ctx context.Context, op string, resp *http.Response, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("github: %s: %w", op, ctx.Err())
	}
	kind := ErrInvalidRequest
	var netErr net.Error
	if errors.As(err, &netErr) {
		kind = ErrUnavailable
	}
	return &APIError{Op: op, Status: resp.StatusCode, RequestID: requestID(resp.Header), kind: kind}
}

// statusError maps a failed reply to an *APIError. The body is read, up to
// a bound, and thrown away: GitHub's messages quote the request, and a
// misbehaving server could echo the token. A redirect is never followed
// (see refuseRedirect), and asking again will not change where it points,
// so it is ErrInvalidRequest.
func statusError(op string, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	e := &APIError{Op: op, Status: resp.StatusCode, RequestID: requestID(resp.Header)}
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized:
		e.kind = ErrAuth
	case s == http.StatusTooManyRequests || (s == http.StatusForbidden && rateLimited(resp.Header)):
		e.kind = ErrRateLimited
		e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
	case s == http.StatusNotFound:
		e.kind = ErrNotFound
	case s == http.StatusRequestTimeout || s >= 500:
		e.kind = ErrUnavailable
	default:
		// Other 4xx, and 3xx. A 403 without rate-limit headers is a
		// missing permission, which a retry cannot grant.
		e.kind = ErrInvalidRequest
	}
	return e
}

// rateLimited reports GitHub's signs of a primary limit (no requests
// remaining) or a secondary one (a Retry-After on a 403).
func rateLimited(h http.Header) bool {
	return h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != ""
}

// retryAfter reads a Retry-After header in seconds, the form GitHub sends.
// Anything else is ignored, leaving the interval to the retry policy.
func retryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func requestID(h http.Header) string {
	id := h.Get("X-GitHub-Request-Id")
	if !requestIDPattern.MatchString(id) {
		return ""
	}
	return id
}

// send is do with repo's installation token. A token GitHub rejects is
// dropped from the cache, so the activity's retry mints a new one rather
// than presenting the rejected token until it expires.
func (a *App) send(ctx context.Context, repo RepoRef, tok secret, op, method, path string, in, out any) error {
	err := a.do(ctx, op, method, path, tok, in, out)
	if errors.Is(err, ErrAuth) {
		a.invalidate(repo.ID)
	}
	return err
}

// repoPath is the API path under repo. The full name has passed
// checkFullName, whose characters need no escaping in a path.
func repoPath(repo RepoRef, format string, args ...any) string {
	return "/repos/" + repo.FullName + fmt.Sprintf(format, args...)
}

// permanent lists the errors a retry cannot fix: the same request fails
// the same way again.
var permanent = []error{ErrRefused, ErrInvalidRequest, ErrNotFound, ErrInvalidConfig}

// IsPermanent reports whether err is one a retry cannot fix: a refusal by
// this package, a request GitHub rejected as made, a missing resource, bad
// configuration, or a push the remote refused on its merits. ErrAuth is
// not permanent: the retry mints a new token. Ticket 8's activities return
// a permanent error as non-retryable
// (temporal.NewNonRetryableApplicationError); this package does not import
// Temporal, so the wrapping lives with the activity.
func IsPermanent(err error) bool {
	for _, p := range permanent {
		if errors.Is(err, p) {
			return true
		}
	}
	var pe *PushError
	if errors.As(err, &pe) {
		switch pe.Reason {
		case PushNonFastForward, PushStaleLease, PushRemoteRejected:
			return true
		}
	}
	return false
}
