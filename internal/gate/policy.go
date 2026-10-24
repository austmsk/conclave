package gate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

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
	// Paths are the patterns (see match.go) naming test files.
	Paths []string

	// CaseMarkers match the line that declares one test case. A marker
	// counts only where it matches at the line's first non-blank
	// character, so a case commented out with any line-comment syntax
	// counts as removed.
	CaseMarkers []*regexp.Regexp

	// SkipMarkers match anywhere in an added test-file line.
	SkipMarkers []*regexp.Regexp

	// ConfigPaths are patterns naming test configuration; any change to
	// them fails the gate.
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
	// attempt (FR-SEC29). Zero means defaultFlagRejectionsAt.
	FlagRejectionsAt int

	// compiled holds the patterns compiled by LoadConfig. A Policy built
	// any other way compiles them when a gate first needs them.
	compiled *policyPatterns
}

// policyPatterns are a policy's path patterns, defaults included, with
// the lists they were compiled from.
type policyPatterns struct {
	protected, surface, tests, testConfig []pattern
	from                                  [4][]string
}

// defaultFlagRejectionsAt is FlagRejectionsAt when the policy leaves it
// unset.
const defaultFlagRejectionsAt = 3

// placeholderRepo is the key deploy/gates.yaml ships with. It fails
// validation, so the worker refuses to start until the owner fills it in.
const placeholderRepo = "owner/repo"

// fullName is a GitHub owner/repo, as in agent.LoadToolchains.
var fullName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// placeholderSHA256 is the gitleaks hash deploy/gates.yaml ships with.
const placeholderSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"

// gatesFile is the shape of deploy/gates.yaml. Example:
//
//	gitleaks:
//	  path: /usr/local/bin/gitleaks
//	  version: 8.28.0
//	  sha256: <64 lowercase hex digits>
//	repositories:
//	  austmsk/election-tally:
//	    mode: reset_in_place        # optional; clean_room forces it
//	    limits:
//	      max_changed_lines: 400
//	      max_changed_files: 20
//	      max_diff_bytes: 262144
//	    protected_paths: []         # additions to the defaults
//	    execution_surface_paths: [] # additions to the defaults
//	    tests:
//	      paths: [src/*Test.java]   # required
//	      case_markers: ['@Test\b'] # RE2
//	      skip_markers: ['@Disabled'] # RE2
//	      config_paths: [lib/]
//	    flag_rejections_at: 3       # optional, default 3
type gatesFile struct {
	Gitleaks     *gitleaksFile       `yaml:"gitleaks"`
	Repositories map[string]repoFile `yaml:"repositories"`
}

type gitleaksFile struct {
	Path    string `yaml:"path"`
	Version string `yaml:"version"`
	SHA256  string `yaml:"sha256"`
}

type repoFile struct {
	Mode                  string      `yaml:"mode"`
	Limits                *limitsFile `yaml:"limits"`
	ProtectedPaths        []string    `yaml:"protected_paths"`
	ExecutionSurfacePaths []string    `yaml:"execution_surface_paths"`
	Tests                 *testsFile  `yaml:"tests"`
	FlagRejectionsAt      *int        `yaml:"flag_rejections_at"`
}

type limitsFile struct {
	MaxChangedLines int   `yaml:"max_changed_lines"`
	MaxChangedFiles int   `yaml:"max_changed_files"`
	MaxDiffBytes    int64 `yaml:"max_diff_bytes"`
}

type testsFile struct {
	Paths       []string `yaml:"paths"`
	CaseMarkers []string `yaml:"case_markers"`
	SkipMarkers []string `yaml:"skip_markers"`
	ConfigPaths []string `yaml:"config_paths"`
}

// LoadConfig reads and validates deploy/gates.yaml. Unknown fields are an
// error, so a misspelt key stops startup instead of silently leaving a
// gate unconfigured. Patterns and markers are compiled here, once. Every
// failure wraps ErrInvalidConfig and names the offending key.
func LoadConfig(r io.Reader) (Config, error) {
	var f gatesFile
	if err := decodeGates(r, &f); err != nil {
		return Config{}, err
	}
	pin, err := gitleaksPin(f.Gitleaks)
	if err != nil {
		return Config{}, fmt.Errorf("%w: gitleaks.%w", ErrInvalidConfig, err)
	}
	if len(f.Repositories) == 0 {
		return Config{}, fmt.Errorf("%w: repositories: none configured", ErrInvalidConfig)
	}
	cfg := Config{Gitleaks: pin, Repositories: make(map[string]Policy, len(f.Repositories))}
	seen := make(map[string]string, len(f.Repositories))
	for _, name := range slices.Sorted(maps.Keys(f.Repositories)) {
		if err := validateRepoName(name); err != nil {
			return Config{}, fmt.Errorf("%w: repositories.%q: %w", ErrInvalidConfig, name, err)
		}
		// GitHub names are case-insensitive, so two keys that differ only
		// in case would be one repository with two configurations.
		if other, dup := seen[strings.ToLower(name)]; dup {
			return Config{}, fmt.Errorf("%w: repositories.%q: same repository as %q", ErrInvalidConfig, name, other)
		}
		seen[strings.ToLower(name)] = name
		p, err := repoPolicy(f.Repositories[name])
		if err != nil {
			return Config{}, fmt.Errorf("%w: repositories.%q.%w", ErrInvalidConfig, name, err)
		}
		cfg.Repositories[name] = p
	}
	return cfg, nil
}

// decodeGates decodes exactly one YAML document, rejecting unknown keys.
func decodeGates(r io.Reader, v any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("gates configuration: reading: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: parsing YAML: %w", ErrInvalidConfig, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: more than one YAML document", ErrInvalidConfig)
	}
	return nil
}

func gitleaksPin(g *gitleaksFile) (GitleaksPin, error) {
	switch {
	case g == nil:
		return GitleaksPin{}, errors.New("path/version/sha256: missing; pin the gitleaks binary")
	case !filepath.IsAbs(g.Path) || filepath.Clean(g.Path) != g.Path:
		return GitleaksPin{}, errors.New("path: must be an absolute, clean path to the gitleaks binary")
	case strings.TrimSpace(g.Version) == "":
		return GitleaksPin{}, errors.New("version: required")
	case !sha256HexRE.MatchString(g.SHA256):
		return GitleaksPin{}, errors.New("sha256: must be 64 lowercase hex digits")
	case g.SHA256 == placeholderSHA256:
		return GitleaksPin{}, errors.New("sha256: placeholder: put the SHA-256 of the installed gitleaks binary")
	}
	return GitleaksPin{Path: g.Path, Version: g.Version, SHA256: g.SHA256}, nil
}

func validateRepoName(name string) error {
	switch {
	case name == placeholderRepo:
		return errors.New("placeholder: replace owner/repo with the repository's full name")
	case !fullName.MatchString(name):
		return errors.New("not a repository full name of the form owner/repo")
	case strings.HasSuffix(name, "/.") || strings.HasSuffix(name, "/.."):
		return errors.New("not a valid repository name")
	}
	return nil
}

// repoPolicy validates one repository's entry. Errors name the key below
// the repository.
func repoPolicy(rf repoFile) (Policy, error) {
	mode, err := policyMode(rf.Mode)
	if err != nil {
		return Policy{}, err
	}
	lim, err := sizeLimits(rf.Limits)
	if err != nil {
		return Policy{}, err
	}
	tests, err := loadTestPolicy(rf.Tests)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{
		Mode:                  mode,
		Limits:                lim,
		ProtectedPaths:        rf.ProtectedPaths,
		ExecutionSurfacePaths: rf.ExecutionSurfacePaths,
		Tests:                 tests,
		FlagRejectionsAt:      defaultFlagRejectionsAt,
	}
	if rf.FlagRejectionsAt != nil {
		if *rf.FlagRejectionsAt < 1 {
			return Policy{}, errors.New("flag_rejections_at: must be at least 1")
		}
		p.FlagRejectionsAt = *rf.FlagRejectionsAt
	}
	compiled, err := p.compile()
	if err != nil {
		return Policy{}, err
	}
	p.compiled = compiled
	return p, nil
}

func policyMode(m string) (contracts.GatingMode, error) {
	switch mode := contracts.GatingMode(m); mode {
	case "", contracts.GatingResetInPlace, contracts.GatingCleanRoom:
		return mode, nil
	}
	return "", fmt.Errorf("mode: %q is not %s or %s", m, contracts.GatingResetInPlace, contracts.GatingCleanRoom)
}

func sizeLimits(l *limitsFile) (SizeLimits, error) {
	switch {
	case l == nil:
		return SizeLimits{}, errors.New("limits: required")
	case l.MaxChangedLines <= 0:
		return SizeLimits{}, errors.New("limits.max_changed_lines: required and greater than zero")
	case l.MaxChangedFiles <= 0:
		return SizeLimits{}, errors.New("limits.max_changed_files: required and greater than zero")
	case l.MaxDiffBytes <= 0:
		return SizeLimits{}, errors.New("limits.max_diff_bytes: required and greater than zero")
	}
	return SizeLimits{MaxChangedLines: l.MaxChangedLines, MaxChangedFiles: l.MaxChangedFiles, MaxDiffBytes: l.MaxDiffBytes}, nil
}

func loadTestPolicy(t *testsFile) (TestPolicy, error) {
	if t == nil || len(t.Paths) == 0 {
		return TestPolicy{}, errors.New("tests.paths: required; the gate-weakening gate needs to know what a test file is")
	}
	cases, err := compileMarkers("tests.case_markers", t.CaseMarkers)
	if err != nil {
		return TestPolicy{}, err
	}
	skips, err := compileMarkers("tests.skip_markers", t.SkipMarkers)
	if err != nil {
		return TestPolicy{}, err
	}
	return TestPolicy{Paths: t.Paths, CaseMarkers: cases, SkipMarkers: skips, ConfigPaths: t.ConfigPaths}, nil
}

// compileMarkers compiles RE2 markers; an empty marker would match every
// line.
func compileMarkers(key string, raws []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(raws))
	for i, raw := range raws {
		if raw == "" {
			return nil, fmt.Errorf("%s[%d]: empty", key, i)
		}
		re, err := regexp.Compile(raw)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("%s[%d]: matches an empty line", key, i)
		}
		out = append(out, re)
	}
	return out, nil
}

// compile builds the policy's patterns, defaults included. Errors name
// the key that held the bad pattern.
func (p Policy) compile() (*policyPatterns, error) {
	var c policyPatterns
	for i, src := range p.patternSources() {
		c.from[i] = slices.Clone(src)
	}
	var err error
	if c.protected, err = compilePatterns(defaultProtectedPaths, p.ProtectedPaths); err != nil {
		return nil, fmt.Errorf("protected_paths: %w", err)
	}
	if c.surface, err = compilePatterns(defaultExecutionSurfacePaths, p.ExecutionSurfacePaths); err != nil {
		return nil, fmt.Errorf("execution_surface_paths: %w", err)
	}
	if c.tests, err = compilePatterns(p.Tests.Paths); err != nil {
		return nil, fmt.Errorf("tests.paths: %w", err)
	}
	if c.testConfig, err = compilePatterns(p.Tests.ConfigPaths); err != nil {
		return nil, fmt.Errorf("tests.config_paths: %w", err)
	}
	return &c, nil
}

// patterns returns the compiled patterns, compiling them again if the
// policy did not come from LoadConfig or its lists changed since.
func (p Policy) patterns() (*policyPatterns, error) {
	if p.compiled != nil && p.compiled.compiledFrom(p.patternSources()) {
		return p.compiled, nil
	}
	return p.compile()
}

// compiledFrom reports whether c was compiled from exactly these lists.
func (c *policyPatterns) compiledFrom(src [4][]string) bool {
	for i := range src {
		if !slices.Equal(c.from[i], src[i]) {
			return false
		}
	}
	return true
}

// patternSources are the configured lists the patterns compile from.
func (p Policy) patternSources() [4][]string {
	return [4][]string{p.ProtectedPaths, p.ExecutionSurfacePaths, p.Tests.Paths, p.Tests.ConfigPaths}
}

// Constraints returns the policy as the contracts the agent is told about:
// the defaults followed by this repository's additions.
func (p Policy) Constraints() contracts.Constraints {
	return contracts.Constraints{
		MaxChangedLines:       p.Limits.MaxChangedLines,
		ProtectedPaths:        slices.Concat(defaultProtectedPaths, p.ProtectedPaths),
		ExecutionSurfacePaths: slices.Concat(defaultExecutionSurfacePaths, p.ExecutionSurfacePaths),
	}
}
