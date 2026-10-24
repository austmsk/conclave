package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
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

// Exit codes the shell reserves for a command it could not run: 126 found
// but not executable, 127 not found. Either means the image or the
// configured command is wrong, not the attempt.
const (
	exitNotExecutable = 126
	exitNotFound      = 127
)

// classifyExec sorts the outcome of one gate command (decision 10). It
// returns:
//
//   - ("", nil) when the command passed: exit 0, no timeout, no error;
//   - (detail, nil) when it failed through the attempt's own fault: a
//     non-zero exit, a timeout, or output past the backend's limit.
//     detail is written by the platform and never quotes the sandbox;
//   - (_, err) when the gate cannot be decided. err wraps the caller's
//     context error when the caller gave up, so cancellation is never
//     mistaken for a broken environment; otherwise it wraps ErrEnvironment.
//     Every Exec error but sandbox.ErrOutputTooLarge lands here: a killed,
//     closed or expired sandbox and a lost daemon connection alike. Exit
//     codes 126 and 127 do too.
//
// what names the command for the error message.
func classifyExec(ctx context.Context, what string, res contracts.ExecResult, err error) (string, error) {
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("running %s: %w", what, ctxErr)
		}
		if errors.Is(err, sandbox.ErrOutputTooLarge) {
			return "output exceeded limit", nil
		}
		return "", fmt.Errorf("%w: running %s: %w", ErrEnvironment, what, err)
	}
	switch {
	case res.TimedOut:
		return "timed out after " + res.Duration.Round(time.Second).String(), nil
	case res.ExitCode == exitNotExecutable || res.ExitCode == exitNotFound:
		return "", environmentf("running %s: exit %d: command not found or not executable", what, res.ExitCode)
	case res.ExitCode != 0:
		return fmt.Sprintf("exit %d", res.ExitCode), nil
	}
	return "", nil
}

// stepError reports a platform step (wiping, extracting, a report read)
// that could not run. Any Exec error is the environment's, output limits
// included, since platform steps produce little output; the caller's own
// cancellation is reported as such.
func stepError(ctx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", what, ctxErr)
	}
	return fmt.Errorf("%w: %s: %w", ErrEnvironment, what, err)
}
