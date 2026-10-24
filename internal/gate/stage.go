package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// Fixed paths staging uses inside the sandbox. WriteFile is relative to
// the workspace, so each upload lands there first under a name the
// prepared tree is checked not to use, and is moved to /tmp before
// anything else touches the workspace.
const (
	stageTarName  = ".conclave-stage.tar"
	stageDiffName = ".conclave-stage.diff"
	stageTarPath  = "/tmp/conclave-stage.tar"
	stageDiffPath = "/tmp/conclave-stage.diff"
)

// maxStageTreeBytes caps the archive of the prepared tree, which staging
// holds in worker memory and the sandbox holds in /tmp while it unpacks.
// On the Docker backend /tmp is an eighth of Limits.DiskBytes, so that,
// not this cap, is usually what bounds the tree; a tree that does not fit
// fails as ErrEnvironment, since no retry of the attempt can fix it.
const maxStageTreeBytes = 256 << 20

// isolatedGit runs git with no system or global configuration and no
// home, so nothing outside the re-streamed .git can configure it. The
// repository's own config comes from the prepared tree, which
// sandbox.VerifyTree has checked holds only core and extensions.
// ExecRequest has no environment field, so env(1) sets it in the argv.
var isolatedGit = []string{"env", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent"}

// wipeScript empties the workspace, .git and dotfiles included, and /tmp,
// then proves both are empty. chmod first, so a directory the agent made
// unreadable or unwritable cannot survive the rm. /tmp itself belongs to
// root and cannot be chmodded; its entries all belong to the sandbox
// user, so the error is expected and ignored. /home/agent is kept on
// purpose (accepted threat T8a).
const wipeScript = `chmod -R u+rwX /workspace /tmp 2>/dev/null
find /workspace /tmp -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
left=$(find /workspace /tmp -mindepth 1 | head -n 5)
[ -z "$left" ] || { echo "not empty after wipe: $left" >&2; exit 1; }`

// extractScript moves the uploaded archive out of the workspace, which
// must then be empty again, unpacks it as the sandbox user, and deletes
// it. --no-same-owner because the archive's owner fields mean nothing
// here: every file belongs to whoever unpacks it.
const extractScript = `set -e
mv -- "$1" "$2"
[ -z "$(find /workspace -mindepth 1 | head -n 1)" ]
tar -x --no-same-owner -f "$2" -C /workspace
rm -f -- "$2"`

// roundTripScript is the command Diff runs (internal/sandbox/docker,
// Sandbox.Diff) plus --no-textconv. It runs under isolatedGit.
const roundTripScript = `git add -A -N . && exec git diff --binary --no-color --no-ext-diff --no-textconv "$1"`

// stage replaces the sandbox's workspace with the prepared tree at
// treePath plus the recorded diff, and reports whether the diff
// reproduces exactly on it. It is the same for both gating modes
// (decision 3): the agent's workspace, its .git and /tmp are wiped, the
// clean tree is streamed back in from the worker, the recorded bytes are
// applied with git configuration isolated, and a fresh diff must equal
// them byte for byte.
//
// reproduced is false, with a nil error, when the diff does not apply or
// the round trip differs: that is the attempt's own failure. An error
// wraps ErrEnvironment when staging itself could not be done (an upload,
// a wipe that leaves something, a tree whose HEAD is not art.Base, a diff
// that does not hash to art.Hash), or wraps the context's error when ctx
// ended.
//
// The sandbox needs sh, env, find, mv, rm, chmod, head, tar and git. Its
// /tmp must hold the archived tree while it unpacks (see
// maxStageTreeBytes).
func stage(ctx context.Context, sb contracts.Sandbox, treePath string, art Artifact, diff []byte) (reproduced bool, err error) {
	// What is staged must be the artifact the results will name.
	if HashArtifact(art.Base, diff) != art.Hash {
		return false, environmentf("staging artifact %s: diff does not match the artifact hash", art.Hash)
	}
	tar, err := archiveTree(treePath, art.Base)
	if err != nil {
		return false, err
	}
	if err := wipe(ctx, sb); err != nil {
		return false, err
	}
	if err := unpackTree(ctx, sb, tar); err != nil {
		return false, err
	}
	applied, err := applyDiff(ctx, sb, diff)
	if err != nil || !applied {
		return false, err
	}
	return roundTrip(ctx, sb, art.Base, diff)
}

// wipe empties /workspace and /tmp and checks they are empty.
func wipe(ctx context.Context, sb contracts.Sandbox) error {
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: []string{"sh", "-c", wipeScript}})
	if err != nil {
		return stepError(ctx, "wiping sandbox "+sb.ID(), err)
	}
	if res.ExitCode != 0 {
		return environmentf("wiping sandbox %s: exit %d", sb.ID(), res.ExitCode)
	}
	return nil
}

// unpackTree uploads the archived prepared tree and unpacks it into the
// empty workspace.
func unpackTree(ctx context.Context, sb contracts.Sandbox, tar []byte) error {
	if err := sb.WriteFile(ctx, stageTarName, tar); err != nil {
		return stepError(ctx, "uploading prepared tree", err)
	}
	res, err := sb.Exec(ctx, contracts.ExecRequest{
		Command: []string{"sh", "-c", extractScript, "sh", "/workspace/" + stageTarName, stageTarPath},
	})
	if err != nil {
		return stepError(ctx, "unpacking prepared tree", err)
	}
	if res.ExitCode != 0 {
		return environmentf("unpacking prepared tree: exit %d", res.ExitCode)
	}
	return nil
}

// archiveTree verifies the prepared tree, checks its HEAD is the
// artifact's base, and returns it as a tar archive. The tree must not use
// the upload names, or unpacking it would overwrite the upload.
func archiveTree(treePath, base string) ([]byte, error) {
	if err := sandbox.VerifyTree(treePath); err != nil {
		return nil, environmentf("prepared tree %s: %v", treePath, err)
	}
	head, err := sandbox.HeadCommit(treePath)
	if err != nil {
		return nil, environmentf("prepared tree %s: %v", treePath, err)
	}
	if head != base {
		return nil, environmentf("prepared tree %s is at %s, artifact base is %s", treePath, head, base)
	}
	for _, name := range []string{stageTarName, stageDiffName} {
		if _, err := os.Lstat(filepath.Join(treePath, name)); !errors.Is(err, os.ErrNotExist) {
			return nil, environmentf("prepared tree %s holds %s, which staging reserves", treePath, name)
		}
	}
	buf := &cappedBuffer{limit: maxStageTreeBytes}
	if err := sandbox.WriteTree(buf, treePath, 0, 0); err != nil {
		return nil, environmentf("archiving prepared tree %s: %v", treePath, err)
	}
	return buf.Bytes(), nil
}

// applyDiff uploads the recorded diff and applies it with git
// configuration isolated. It reports false, with no error, when git
// refuses the diff.
func applyDiff(ctx context.Context, sb contracts.Sandbox, diff []byte) (bool, error) {
	if err := sb.WriteFile(ctx, stageDiffName, diff); err != nil {
		return false, stepError(ctx, "uploading recorded diff", err)
	}
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: []string{"mv", "--", "/workspace/" + stageDiffName, stageDiffPath}})
	if err != nil {
		return false, stepError(ctx, "moving recorded diff", err)
	}
	if res.ExitCode != 0 {
		return false, environmentf("moving recorded diff: exit %d", res.ExitCode)
	}
	if len(diff) == 0 {
		// git apply refuses empty input; an empty diff applies as nothing
		// and the round trip decides.
		return true, nil
	}
	cmd := append(append([]string{}, isolatedGit...), "git", "apply", "--binary", stageDiffPath)
	res, err = sb.Exec(ctx, contracts.ExecRequest{Command: cmd})
	if err != nil {
		return false, stepError(ctx, "applying recorded diff", err)
	}
	if res.ExitCode == exitNotExecutable || res.ExitCode == exitNotFound {
		return false, environmentf("applying recorded diff: exit %d: git or env missing from the image", res.ExitCode)
	}
	return res.ExitCode == 0, nil
}

// roundTrip diffs the staged workspace exactly as Diff would, without
// textconv, and compares the bytes with the recorded diff. Output past
// the backend's limit cannot equal a diff Freeze accepted, so it counts
// as a mismatch rather than an environment failure.
func roundTrip(ctx context.Context, sb contracts.Sandbox, base string, diff []byte) (bool, error) {
	cmd := append(append([]string{}, isolatedGit...), "sh", "-c", roundTripScript, "sh", base)
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: cmd})
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, sandbox.ErrOutputTooLarge) {
			return false, nil
		}
		return false, stepError(ctx, "re-diffing staged workspace", err)
	}
	if res.ExitCode != 0 {
		// stderr is not quoted: it can carry names from the attacker's diff.
		return false, environmentf("re-diffing staged workspace: git exited %d", res.ExitCode)
	}
	return bytes.Equal(res.Stdout, diff), nil
}

// cappedBuffer is a bytes.Buffer that refuses to grow past limit.
type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("archive exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}
