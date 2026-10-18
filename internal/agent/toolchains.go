package agent

import (
	"errors"
	"io"
)

// ErrInvalidToolchains wraps every toolchains.yaml validation failure.
var ErrInvalidToolchains = errors.New("invalid toolchains configuration")

// LoadToolchains reads deploy/toolchains.yaml: each repository's
// configured commands, keyed by full name as in deploy/repos.yaml.
func LoadToolchains(r io.Reader) (map[string]Commands, error) {
	return nil, errors.New("not implemented")
}
