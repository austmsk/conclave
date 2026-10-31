package gate

import (
	"context"
	"errors"
	"fmt"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// ErrRebaseConflict means the recorded diff does not apply to the new
// base, even three-way. It is the work's own problem, not the
// environment's: the feature request stops for the owner (decision
// 0014), and no artifact is produced.
var ErrRebaseConflict = errors.New("diff does not apply to the new base")

// Paths Rebase uses inside the sandbox, beside staging's. The upload
// lands in the workspace under a reserved name and is moved to /tmp
// before git runs, so the diff itself is never part of the diff.
const (
	rebaseDiffName = ".conclave-rebase.diff"
	rebaseDiffPath = "/tmp/conclave-rebase.diff"
)

// threeWayScript refreshes the index, then applies the diff three-way.
// The tree was copied into the sandbox, so every index entry's stat
// information is stale, and git apply would refuse each file it touches
// as not matching the index. The refresh's own exit status is ignored: it
// reports files that differ, and in a fresh sandbox none do; any that did
// would fail the apply. It runs under isolatedGit.
const threeWayScript = `git update-index -q --refresh >/dev/null 2>&1
exec git apply --3way --binary "$1"`

// Rebase moves an artifact onto newBase: the finalization rebase of
// FR-S19 and FR-A5. sb is a fresh sandbox the caller created over a
// prepared tree whose HEAD is newBase, with no agent ever in it; the
// recorded diff is applied there with git apply --3way under isolated
// configuration, and the result is frozen against newBase.
//
// The result is a new artifact, with a new hash, that must be gated
// again before anything pushes it (FR-SEC27): Rebase only produces it.
// git runs in the sandbox rather than on the host, so the diff bytes are
// the ones the gate's round trip in the same image reproduces.
//
// When newBase is the artifact's own base there is nothing to move:
// original comes back unchanged and sb is not touched. A diff that does
// not apply is ErrRebaseConflict; git's output is never quoted, since it
// names files from the agent's diff. A sandbox that cannot run git, an
// upload or a freeze that fails, is ErrEnvironment.
func Rebase(ctx context.Context, sb contracts.Sandbox, blobs blob.Store, original Artifact, diff []byte, newBase string) (Artifact, error) {
	switch {
	case HashArtifact(original.Base, diff) != original.Hash:
		return Artifact{}, fmt.Errorf("rebasing artifact %s: diff does not match the artifact hash", original.Hash)
	case !baseRE.MatchString(newBase):
		return Artifact{}, fmt.Errorf("rebasing artifact %s: new base %q is not a full commit ID", original.Hash, newBase)
	case newBase == original.Base:
		return original, nil
	}
	if len(diff) > 0 {
		if err := applyThreeWay(ctx, sb, diff); err != nil {
			return Artifact{}, fmt.Errorf("rebasing artifact %s onto %s: %w", original.Hash, newBase, err)
		}
	}
	rebased, err := Freeze(ctx, sb, blobs, original.RepoID, newBase)
	if err != nil {
		return Artifact{}, fmt.Errorf("rebasing artifact %s onto %s: %w", original.Hash, newBase, err)
	}
	return rebased, nil
}

// applyThreeWay uploads diff and applies it to the sandbox's workspace
// with a three-way merge, which finds the preimage blobs in the tree's
// own history.
func applyThreeWay(ctx context.Context, sb contracts.Sandbox, diff []byte) error {
	if err := sb.WriteFile(ctx, rebaseDiffName, diff); err != nil {
		return stepError(ctx, "uploading recorded diff", err)
	}
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: []string{"mv", "--", "/workspace/" + rebaseDiffName, rebaseDiffPath}})
	if err != nil {
		return stepError(ctx, "moving recorded diff", err)
	}
	if res.ExitCode != 0 {
		return environmentf("moving recorded diff: exit %d", res.ExitCode)
	}
	cmd := append(append([]string{}, isolatedGit...), "sh", "-c", threeWayScript, "sh", rebaseDiffPath)
	res, err = sb.Exec(ctx, contracts.ExecRequest{Command: cmd})
	if err != nil {
		return stepError(ctx, "applying recorded diff three-way", err)
	}
	switch res.ExitCode {
	case 0:
		return nil
	case exitNotExecutable, exitNotFound:
		return environmentf("applying recorded diff three-way: exit %d: git or env missing from the image", res.ExitCode)
	}
	return fmt.Errorf("git apply --3way exited %d: %w", res.ExitCode, ErrRebaseConflict)
}
