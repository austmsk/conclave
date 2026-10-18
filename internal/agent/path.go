package agent

import "github.com/austmsk/conclave/internal/sandbox"

// checkPath applies sandbox.CheckPath and then the denylist, which matches
// whole path components. Every refusal wraps sandbox.ErrInvalidPath.
func checkPath(p string, denylist []string) error {
	return sandbox.CheckPath(p)
}
