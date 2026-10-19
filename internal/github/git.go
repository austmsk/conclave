package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

// Clone clones repo at ref into dir through sandbox.Clone, with a token
// scoped to that repository.
//
// L0 stub: W3 implements.
func (a *App) Clone(ctx context.Context, repo RepoRef, dir, ref string) error {
	_, _, _, _ = ctx, repo, dir, ref
	return errors.New("github: Clone not implemented")
}

// Push pushes p.Commit to p.Branch. The refspec is built here, never taken
// from the caller, and git runs with a scrubbed environment, the token in
// a header scoped to the repository's URL, no hooks, and no redirects.
//
// L0 stub: W3 implements.
func (a *App) Push(ctx context.Context, p PushSpec) error {
	_, _ = ctx, p
	return errors.New("github: Push not implemented")
}
