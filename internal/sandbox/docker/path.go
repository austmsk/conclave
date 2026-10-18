package docker

import "github.com/austmsk/conclave/internal/sandbox"

// ErrInvalidPath is sandbox.ErrInvalidPath, kept here so callers that
// know they hold a Docker sandbox can name it from this package.
var ErrInvalidPath = sandbox.ErrInvalidPath

// workspacePath validates p with sandbox.CheckPath and returns it
// unchanged.
func workspacePath(p string) (string, error) {
	if err := sandbox.CheckPath(p); err != nil {
		return "", err
	}
	return p, nil
}
