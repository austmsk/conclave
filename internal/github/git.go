package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/austmsk/conclave/internal/sandbox"
)

// PushSpec is one commit to push to one agent/ branch.
type PushSpec struct {
	Repo RepoRef
	// Dir is a prepared tree holding Commit (sandbox.VerifyTree must pass).
	Dir string
	// Commit is the full commit ID to push.
	Commit string
	// Branch is the target, which must pass CheckBranch.
	Branch string
	// ExpectedOld is the commit the branch must point at before the push,
	// or empty when the branch must not exist yet. Every push carries this
	// lease, so a push never overwrites work it has not seen.
	ExpectedOld string
}

// PushFailure classifies a refused push.
type PushFailure string

const (
	PushNonFastForward PushFailure = "non_fast_forward"
	PushStaleLease     PushFailure = "stale_lease"
	PushRemoteRejected PushFailure = "remote_rejected"
	PushAuth           PushFailure = "auth"
	PushOther          PushFailure = "other"
)

// PushError is a push git or GitHub refused. Detail is git's or GitHub's
// reason, redacted and truncated; it is kept for remote_rejected because
// GitHub's refusal of a workflow file edit is the message that explains
// it.
type PushError struct {
	Branch string
	Reason PushFailure
	Detail string
}

func (e *PushError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("github: push to %s: %s", e.Branch, e.Reason)
	}
	return fmt.Sprintf("github: push to %s: %s: %s", e.Branch, e.Reason, e.Detail)
}

// agentBranch is the only shape of branch the App pushes to: an issue's
// integration branch or one of its pull request branches (FR-S16,
// FR-SEC16).
var agentBranch = regexp.MustCompile(`^agent/[1-9][0-9]{0,9}/(integration|pr-[1-9][0-9]{0,2})$`)

// CheckBranch returns an error wrapping ErrRefused unless name is
// agent/<issue>/integration or agent/<issue>/pr-<n>. The pattern admits
// nothing git would read as an option, a refspec, a ref outside
// refs/heads, or a path that leaves agent/.
func CheckBranch(name string) error {
	if !agentBranch.MatchString(name) {
		return fmt.Errorf("%w: branch %q is not an agent/ branch", ErrRefused, name)
	}
	return nil
}

// commitID is a full commit ID, SHA-1 or SHA-256. Nothing shorter is
// pushed: an abbreviation, a branch name, or HEAD would be resolved by git
// in a tree the caller controls.
var commitID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

const (
	// maxDetail caps PushError.Detail, in bytes.
	maxDetail = 200
	// maxGitOutput caps what is kept of each of git's output streams.
	maxGitOutput = 64 << 10
	// gitWaitDelay bounds the wait for git's children after a cancelled
	// context has killed git itself.
	gitWaitDelay = 10 * time.Second
)

// Clone clones repo at ref into dir through sandbox.Clone, with a token
// scoped to that repository. The URL is built from configuration and the
// checked full name, never taken from a payload.
//
// sandbox.Clone runs git with its own scrubbed environment, which neither
// sets http.followRedirects=false nor scopes the token's header to the
// repository's URL as Push does. That is ticket 3's code and is left as it
// is here.
func (a *App) Clone(ctx context.Context, repo RepoRef, dir, ref string) error {
	if err := checkFullName(repo.FullName); err != nil {
		return err
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return fmt.Errorf("github: cloning %s: %w", repo.FullName, err)
	}
	err = sandbox.Clone(ctx, sandbox.CloneSpec{URL: a.repoURL(repo.FullName), Dir: dir, Ref: ref, Token: tok.reveal()})
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("github: cloning %s: %w", repo.FullName, ctx.Err())
	case errors.Is(err, sandbox.ErrCloneSpec):
		return fmt.Errorf("%w: cloning %s: %s", ErrRefused, repo.FullName, redact(err.Error(), tok, basicAuth(tok)))
	}
	// The error carries git's output, so it is redacted as text rather
	// than wrapped.
	return fmt.Errorf("github: cloning %s: %s", repo.FullName, redact(err.Error(), tok, basicAuth(tok)))
}

// Push pushes p.Commit to p.Branch. The refspec is built here, never taken
// from the caller, and git runs with a scrubbed environment, the token in
// a header scoped to the repository's URL, no hooks, and no redirects.
//
// Every refusal of the spec wraps ErrRefused and happens before a token is
// minted or git runs. A push git or GitHub refuses is a *PushError. A
// repeated push of a commit the branch already holds succeeds, so a retry
// after a push that succeeded unreported is harmless.
func (a *App) Push(ctx context.Context, p PushSpec) error {
	if err := checkPush(p); err != nil {
		return err
	}
	tok, err := a.token(ctx, p.Repo)
	if err != nil {
		return fmt.Errorf("github: push to %s: %w", p.Branch, err)
	}
	remote := a.repoURL(p.Repo.FullName)
	ref := "refs/heads/" + p.Branch
	// An empty lease value means the ref must not exist yet.
	args := []string{
		"push", "--porcelain", "--atomic", "--no-verify", "--recurse-submodules=no",
		"--force-with-lease=" + ref + ":" + p.ExpectedOld,
		remote, p.Commit + ":" + ref,
	}
	out, runErr := runGit(ctx, p.Dir, a.gitConfig(remote, tok), args...)
	if ctx.Err() != nil {
		return fmt.Errorf("github: push to %s: %w", p.Branch, ctx.Err())
	}
	_, _, rejected := porcelainRejection(out.stdout)
	if runErr == nil && !rejected {
		return nil
	}
	return classifyPush(p.Branch, out, tok, basicAuth(tok))
}

// checkPush refuses a spec before anything runs: a branch outside agent/,
// a commit or lease that is not a full commit ID, a malformed repository
// name, or a tree that is not prepared. An unprepared tree could carry a
// remote, an insteadOf, hooks, or credentials that would redirect the
// token or run code with it.
func checkPush(p PushSpec) error {
	if err := CheckBranch(p.Branch); err != nil {
		return err
	}
	if !commitID.MatchString(p.Commit) {
		return fmt.Errorf("%w: commit %q is not a full commit ID", ErrRefused, p.Commit)
	}
	if p.ExpectedOld != "" && !commitID.MatchString(p.ExpectedOld) {
		return fmt.Errorf("%w: expected commit %q is not a full commit ID", ErrRefused, p.ExpectedOld)
	}
	if err := checkFullName(p.Repo.FullName); err != nil {
		return err
	}
	// A relative or empty directory would be resolved against the worker's
	// own working directory, by VerifyTree and by git alike.
	if !filepath.IsAbs(p.Dir) {
		return fmt.Errorf("%w: tree %q is not an absolute path", ErrRefused, p.Dir)
	}
	if err := sandbox.VerifyTree(p.Dir); err != nil {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return nil
}

// repoURL is the repository's git URL, from configuration and a full name
// checkFullName has accepted.
func (a *App) repoURL(fullName string) string {
	return strings.TrimSuffix(a.cfg.GitBaseURL, "/") + "/" + fullName + ".git"
}

// basicAuth is the Basic credential git sends: the token as the password
// of the x-access-token user, base64-encoded. It is redacted alongside the
// token, since it decodes to it.
func basicAuth(tok secret) secret {
	return newSecret(base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok.reveal())))
}

// gitConfig is the configuration git runs with, passed through
// GIT_CONFIG_* variables so it is never written to a file. The token
// travels as a header scoped to remote alone; redirects, hooks, the
// filesystem monitor and credential helpers are off; and only https is
// allowed, plus plain http when the configured base URL is a loopback
// host, as in tests.
func (a *App) gitConfig(remote string, tok secret) []string {
	kv := [][2]string{
		{"http." + remote + ".extraheader", "Authorization: Basic " + basicAuth(tok).reveal()},
		{"http.followRedirects", "false"},
		{"credential.helper", ""},
		{"core.hooksPath", "/dev/null"},
		{"core.fsmonitor", "false"},
		{"protocol.allow", "never"},
		{"protocol.https.allow", "always"},
	}
	if u, err := url.Parse(a.cfg.GitBaseURL); err == nil && u.Scheme == "http" && isLoopback(u.Hostname()) {
		kv = append(kv, [2]string{"protocol.http.allow", "always"})
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(kv))}
	for i, e := range kv {
		env = append(env,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+e[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+e[1],
		)
	}
	return env
}

// gitOutput is what git wrote, each stream capped at maxGitOutput.
type gitOutput struct {
	stdout string
	stderr string
}

// runGit runs git in dir with an environment built from scratch, as
// sandbox's runGit does: nothing of the worker's environment reaches git
// but PATH, and config holds the only settings beyond git's defaults. The
// token is never on the command line. The returned error is git's exit
// status alone; its output is returned for the caller to classify and
// redact.
func runGit(ctx context.Context, dir string, config []string, args ...string) (gitOutput, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"LANG=C",
		"LC_ALL=C",
	}, config...)
	cmd.WaitDelay = gitWaitDelay
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return gitOutput{stdout: stdout.String(), stderr: stderr.String()}, err
}

// cappedBuffer keeps the first maxGitOutput bytes written to it and
// discards the rest, so a remote cannot grow the worker's memory with
// output.
type cappedBuffer struct{ buf bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxGitOutput - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// porcelainRejection finds the line --porcelain prints for a refused ref,
// "!\t<src>:<dst>\t<summary> (<reason>)", and splits its status.
func porcelainRejection(stdout string) (summary, reason string, ok bool) {
	for _, line := range strings.Split(stdout, "\n") {
		flag, rest, found := strings.Cut(line, "\t")
		if !found || flag != "!" {
			continue
		}
		_, status, _ := strings.Cut(rest, "\t")
		summary, reason, _ = strings.Cut(status, " (")
		return summary, strings.TrimSuffix(reason, ")"), true
	}
	return "", "", false
}

// classifyPush turns a failed push into a *PushError. Only a remote
// rejection and an unclassified failure keep a detail, redacted, stripped
// of control characters, and truncated: a remote rejection is where GitHub
// explains a refused workflow file edit.
func classifyPush(branch string, out gitOutput, known ...secret) error {
	pe := &PushError{Branch: branch, Reason: PushOther}
	summary, reason, rejected := porcelainRejection(out.stdout)
	switch {
	case rejected && summary == "[remote rejected]":
		pe.Reason = PushRemoteRejected
		pe.Detail = detail(withRemoteMessage(reason, out.stderr), known)
	case rejected && reason == "stale info":
		pe.Reason = PushStaleLease
	case rejected && (reason == "fetch first" || reason == "non-fast-forward"):
		pe.Reason = PushNonFastForward
	case rejected:
		pe.Detail = detail(summary+" "+reason, known)
	case authFailure(out.stderr):
		pe.Reason = PushAuth
	default:
		pe.Detail = detail(lastLine(out.stderr), known)
	}
	return pe
}

// withRemoteMessage appends what the remote printed ("remote: " lines) to
// the porcelain reason. GitHub puts its explanation in the reason itself;
// a pre-receive hook's explanation arrives in those lines.
func withRemoteMessage(reason, stderr string) string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		msg, ok := strings.CutPrefix(line, "remote:")
		if msg = strings.TrimSpace(msg); ok && msg != "" {
			lines = append(lines, msg)
		}
	}
	if len(lines) == 0 {
		return reason
	}
	return reason + ": " + strings.Join(lines, " ")
}

// authFailure reports whether git's output shows the credential was
// refused: a 401 with no way to prompt, or a 403.
func authFailure(stderr string) bool {
	for _, marker := range []string{"Authentication failed", "could not read Username", "returned error: 401", "returned error: 403"} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// detail redacts s, then makes it one printable line of at most maxDetail
// bytes. Redaction comes first, so truncation cannot leave a token's
// prefix that no longer matches a pattern.
func detail(s string, known []secret) string {
	s = redact(s, known...)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxDetail {
		return s
	}
	cut := maxDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
