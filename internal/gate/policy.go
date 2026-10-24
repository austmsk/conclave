package gate

import (
	"errors"
	"io"
	"regexp"

	"github.com/austmsk/conclave/internal/contracts"
)

// ErrInvalidConfig wraps every gates.yaml validation failure.
var ErrInvalidConfig = errors.New("invalid gates configuration")

// Config is deploy/gates.yaml.
type Config struct {
	Gitleaks     GitleaksPin
	Repositories map[string]Policy
}

// SizeLimits bound a diff. Every field is required; zero is invalid.
type SizeLimits struct {
	MaxChangedLines int
	MaxChangedFiles int
	MaxDiffBytes    int64
}

// TestPolicy tells the gate-weakening gate what a test looks like in one
// repository (FR-I4: nothing language-specific is hardcoded).
type TestPolicy struct {
	Paths       []string
	CaseMarkers []*regexp.Regexp
	SkipMarkers []*regexp.Regexp
	ConfigPaths []string
}

// Policy is one repository's gate configuration. Protected and
// execution-surface paths add to the defaults and never remove any.
type Policy struct {
	// Mode forces clean_room when set to it; empty or reset_in_place
	// leaves the choice to SelectMode.
	Mode contracts.GatingMode

	Limits                SizeLimits
	ProtectedPaths        []string
	ExecutionSurfacePaths []string
	Tests                 TestPolicy

	// FlagRejectionsAt is the rejected tool-call count that flags the
	// attempt (FR-SEC29).
	FlagRejectionsAt int
}

// LoadConfig reads and validates deploy/gates.yaml.
func LoadConfig(r io.Reader) (Config, error) {
	return Config{}, errors.New("not implemented")
}

// Constraints returns the policy as the contracts the agent is told about.
func (p Policy) Constraints() contracts.Constraints {
	return contracts.Constraints{}
}
