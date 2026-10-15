package docker

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidPath reports a workspace path that is rejected rather than
// sanitized (FR-SEC29): absolute, traversing, encoded, or reaching into
// .git internals.
var ErrInvalidPath = errors.New("invalid workspace path")

const maxPathLength = 4096

var percentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// workspacePath validates a caller-supplied path relative to the workspace
// and returns it unchanged. Every rule is a rejection; nothing is
// rewritten, so a path that passes is exactly what the caller asked for.
func workspacePath(p string) (string, error) {
	switch {
	case p == "":
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	case len(p) > maxPathLength:
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidPath, maxPathLength)
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("%w: absolute: %q", ErrInvalidPath, p)
	case strings.HasSuffix(p, "/"):
		return "", fmt.Errorf("%w: trailing slash: %q", ErrInvalidPath, p)
	case strings.ContainsAny(p, "\\\x00"):
		return "", fmt.Errorf("%w: contains a backslash or NUL: %q", ErrInvalidPath, p)
	case hasControlChars(p):
		return "", fmt.Errorf("%w: contains control characters: %q", ErrInvalidPath, p)
	case percentEscape.MatchString(p):
		return "", fmt.Errorf("%w: contains a percent escape: %q", ErrInvalidPath, p)
	}
	for _, component := range strings.Split(p, "/") {
		if err := checkComponent(component); err != nil {
			return "", fmt.Errorf("%w: %q", err, p)
		}
	}
	return p, nil
}

func checkComponent(c string) error {
	switch {
	case c == "":
		return fmt.Errorf("%w: empty component", ErrInvalidPath)
	case c == "." || c == "..":
		return fmt.Errorf("%w: dot component", ErrInvalidPath)
	case strings.Trim(c, ".") == "":
		// "...." and friends are what "....//" collapses to once a
		// naive sanitizer removes "../"; reject the shape outright.
		return fmt.Errorf("%w: dots-only component", ErrInvalidPath)
	case c == ".git":
		return fmt.Errorf("%w: reaches into .git", ErrInvalidPath)
	}
	return nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
