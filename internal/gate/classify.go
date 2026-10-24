package gate

import (
	"errors"
	"fmt"
)

var (
	// ErrEnvironment means the gating environment failed, not the attempt:
	// the attempt is errored, no result rows are written, and a retry
	// starts clean (FR-SEC27).
	ErrEnvironment = errors.New("gating environment failed")

	// ErrArtifactTooLarge means the diff exceeds MaxArtifactBytes. It is
	// the attempt's own failure.
	ErrArtifactTooLarge = errors.New("artifact exceeds size cap")

	// ErrUnstableDiff means two consecutive diff reads disagreed: something
	// still changed the workspace after the agent phase ended.
	ErrUnstableDiff = errors.New("diff changed between reads")
)

// environmentf returns an error wrapping ErrEnvironment.
func environmentf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrEnvironment, fmt.Sprintf(format, a...))
}
