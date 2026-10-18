package sandbox

import "errors"

// Refusals every backend reports the same way, so a caller can tell a
// refusal the agent should see and correct from a failure of the sandbox
// itself. Backends wrap these; they never define their own copies.
var (
	// ErrInvalidPath reports a workspace path that is rejected rather than
	// sanitized (FR-SEC29): absolute, traversing, encoded, reaching into
	// .git internals, or resolving through a symlink or hardlink.
	ErrInvalidPath = errors.New("invalid workspace path")

	// ErrNotFile reports a read or write of something that is not a
	// regular file, or a write whose parent is not a directory.
	ErrNotFile = errors.New("not a regular file")

	// ErrFileTooLarge reports a read of a file larger than the backend's
	// output limit. Nothing is read.
	ErrFileTooLarge = errors.New("file exceeds read limit")

	// ErrOutputTooLarge reports a command whose output passed the
	// backend's limit; the command was killed.
	ErrOutputTooLarge = errors.New("command output exceeds limit")
)
