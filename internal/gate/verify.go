package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// ErrNotVerified means an artifact lacks a passing result for at least one
// required gate. The artifact must not be pushed.
var ErrNotVerified = errors.New("artifact has not passed every required gate")

// Verify is the single check before an artifact is pushed (ticket 9). It
// loads the artifact through blob.Get, which re-hashes it, so a blob
// changed on disk is refused here rather than pushed. It then requires
// that every gate in RequiredGates has a recorded result for this attempt
// whose ArtifactHash is the hash just computed and which passed. A missing
// or failing gate is ErrNotVerified, naming each; a store failure is
// ErrEnvironment.
//
// It returns the artifact as loaded, so the caller pushes what was
// verified rather than looking it up again.
func Verify(ctx context.Context, blobs blob.Store, results Results, attemptID, key string) (Artifact, error) {
	art, _, err := Load(ctx, blobs, key)
	if err != nil {
		return Artifact{}, fmt.Errorf("verifying artifact for attempt %s: %w", attemptID, err)
	}
	rows, err := results.ForArtifact(ctx, attemptID, art.Hash)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: verifying artifact %s for attempt %s: reading gate results: %w",
			ErrEnvironment, art.Hash, attemptID, err)
	}
	missing, failed := unverifiedGates(rows, attemptID, art.Hash)
	if len(missing) == 0 && len(failed) == 0 {
		return art, nil
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "no result for "+strings.Join(missing, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "failed "+strings.Join(failed, ", "))
	}
	return Artifact{}, fmt.Errorf("verifying artifact %s for attempt %s: %w: %s",
		art.Hash, attemptID, ErrNotVerified, strings.Join(parts, "; "))
}

// unverifiedGates returns the required gates with no result for this
// attempt and hash, and those with a result that did not pass. A row for
// another attempt or hash counts for nothing, whatever the store returned.
func unverifiedGates(rows []contracts.GateResult, attemptID, hash string) (missing, failed []string) {
	seen := make(map[contracts.GateName]bool, len(rows))
	notPassed := make(map[contracts.GateName]bool, len(rows))
	for _, r := range rows {
		if r.AttemptID != attemptID || r.ArtifactHash != hash {
			continue
		}
		seen[r.Gate] = true
		// Any failing row fails the gate, even beside a passing one.
		if !r.Passed {
			notPassed[r.Gate] = true
		}
	}
	for _, g := range RequiredGates {
		switch {
		case !seen[g]:
			missing = append(missing, string(g))
		case notPassed[g]:
			failed = append(failed, string(g))
		}
	}
	return missing, failed
}
