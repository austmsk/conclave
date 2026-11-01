package gate

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/deployfile"
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

	// CommentOpeners match a line that leaves a block comment open, and
	// CommentClosers one that closes a comment it did not open. Every run
	// of adjacent lines added to, or removed from, an existing test file
	// must open and close its own comments: a run that leaves one open,
	// or closes one opened elsewhere, fails the gate, because moving a
	// comment boundary disables cases far from any changed line, where
	// neither the case-marker count nor the skip markers see them. Both
	// are set or neither is.
	CommentOpeners []*regexp.Regexp
	CommentClosers []*regexp.Regexp

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

	// CI is when a delivered pull request counts as passing the
	// repository's own CI (FR-A7).
	CI CIPolicy

	// compiled holds the patterns compiled by LoadConfig. A Policy built
	// any other way compiles them when a gate first needs them.
	compiled *policyPatterns
}

// CIPolicy says which of a repository's CI checks a pull request must
// pass before it is marked ready (FR-A7), and how delivery waits for them
// (decision 0014). Delivery reads check runs only, not commit statuses.
type CIPolicy struct {
	// RequiredChecks are check run names. Every one must complete and
	// pass on the pull request's head commit.
	RequiredChecks []string

	// CheckApp is the slug of the GitHub App whose check runs count,
	// github-actions by default. A check run with a required name from
	// any other App is ignored: any App with checks:write on the
	// repository could otherwise satisfy a required check by name.
	CheckApp string

	// PollEvery is how often the checks are read; Timeout is how long
	// delivery waits for them to complete before it stops with
	// ci_timeout.
	PollEvery time.Duration
	Timeout   time.Duration
}

// CI defaults and bounds.
const (
	defaultCheckApp  = "github-actions"
	defaultPollEvery = time.Minute
	defaultCITimeout = time.Hour
	minPollEvery     = 10 * time.Second
	maxCITimeout     = 24 * time.Hour
)

// checkAppRE is the shape of a GitHub App slug.
var checkAppRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// policyPatterns are a policy's path patterns, defaults included, with
// the lists they were compiled from.
type policyPatterns struct {
	protected, surface, tests, testConfig []pattern
	from                                  [4][]string
}

// defaultFlagRejectionsAt is FlagRejectionsAt when the policy leaves it
// unset.
const defaultFlagRejectionsAt = 3

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
//	      comment_openers: ['/\*([^*]|\*+[^*/])*\**$'] # RE2
//	      comment_closers: ['^([^/]|/[^*])*\*/'] # RE2
//	      config_paths: [lib/]
//	    flag_rejections_at: 3       # optional, default 3
//	    ci:                         # required
//	      required_checks: [build]  # check run names, at least one
//	      check_app: github-actions # optional, the default
//	      poll_every: 1m            # optional, default 1m, at least 10s
//	      timeout: 60m              # optional, default 60m, at most 24h
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
	CI                    *ciFile     `yaml:"ci"`
}

type ciFile struct {
	RequiredChecks []string `yaml:"required_checks"`
	CheckApp       string   `yaml:"check_app"`
	PollEvery      string   `yaml:"poll_every"`
	Timeout        string   `yaml:"timeout"`
}

type limitsFile struct {
	MaxChangedLines int   `yaml:"max_changed_lines"`
	MaxChangedFiles int   `yaml:"max_changed_files"`
	MaxDiffBytes    int64 `yaml:"max_diff_bytes"`
}

type testsFile struct {
	Paths          []string `yaml:"paths"`
	CaseMarkers    []string `yaml:"case_markers"`
	SkipMarkers    []string `yaml:"skip_markers"`
	CommentOpeners []string `yaml:"comment_openers"`
	CommentClosers []string `yaml:"comment_closers"`
	ConfigPaths    []string `yaml:"config_paths"`
}

// LoadConfig reads and validates deploy/gates.yaml. Unknown fields are an
// error, so a misspelt key stops startup instead of silently leaving a
// gate unconfigured. Patterns and markers are compiled here, once. Every
// failure wraps ErrInvalidConfig and names the offending key.
func LoadConfig(r io.Reader) (Config, error) {
	var f gatesFile
	if err := deployfile.Decode(r, &f, ErrInvalidConfig); err != nil {
		return Config{}, fmt.Errorf("gates configuration: %w", err)
	}
	pin, err := gitleaksPin(f.Gitleaks)
	if err != nil {
		return Config{}, fmt.Errorf("%w: gitleaks.%w", ErrInvalidConfig, err)
	}
	if len(f.Repositories) == 0 {
		return Config{}, fmt.Errorf("%w: repositories: none configured", ErrInvalidConfig)
	}
	names, err := deployfile.RepoKeys(f.Repositories)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	cfg := Config{Gitleaks: pin, Repositories: make(map[string]Policy, len(f.Repositories))}
	for _, name := range names {
		p, err := repoPolicy(f.Repositories[name])
		if err != nil {
			return Config{}, fmt.Errorf("%w: repositories.%q.%w", ErrInvalidConfig, name, err)
		}
		cfg.Repositories[name] = p
	}
	return cfg, nil
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
	ci, err := ciPolicy(rf.CI)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{
		CI:                    ci,
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

// ciPolicy validates the ci block, which every repository needs: a pull
// request is ready only when the repository's own CI passes (FR-A7).
func ciPolicy(c *ciFile) (CIPolicy, error) {
	if c == nil {
		return CIPolicy{}, errors.New("ci: required; delivery waits for the repository's CI to pass")
	}
	checks, err := requiredChecks(c.RequiredChecks)
	if err != nil {
		return CIPolicy{}, err
	}
	p := CIPolicy{RequiredChecks: checks, CheckApp: defaultCheckApp, PollEvery: defaultPollEvery, Timeout: defaultCITimeout}
	if c.CheckApp != "" {
		if !checkAppRE.MatchString(c.CheckApp) {
			return CIPolicy{}, fmt.Errorf("ci.check_app: %q is not a GitHub App slug", c.CheckApp)
		}
		p.CheckApp = c.CheckApp
	}
	if p.PollEvery, err = ciDuration("ci.poll_every", c.PollEvery, defaultPollEvery); err != nil {
		return CIPolicy{}, err
	}
	if p.Timeout, err = ciDuration("ci.timeout", c.Timeout, defaultCITimeout); err != nil {
		return CIPolicy{}, err
	}
	switch {
	case p.PollEvery < minPollEvery:
		return CIPolicy{}, fmt.Errorf("ci.poll_every: %s is under the minimum %s", p.PollEvery, minPollEvery)
	case p.Timeout < p.PollEvery:
		return CIPolicy{}, fmt.Errorf("ci.timeout: %s is shorter than ci.poll_every %s", p.Timeout, p.PollEvery)
	case p.Timeout > maxCITimeout:
		return CIPolicy{}, fmt.Errorf("ci.timeout: %s is over the maximum %s", p.Timeout, maxCITimeout)
	}
	return p, nil
}

// requiredChecks refuses an empty list, a blank name and a duplicate.
func requiredChecks(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, errors.New("ci.required_checks: required; name at least one check")
	}
	seen := make(map[string]bool, len(names))
	for i, n := range names {
		switch {
		case strings.TrimSpace(n) == "":
			return nil, fmt.Errorf("ci.required_checks[%d]: empty", i)
		case seen[n]:
			return nil, fmt.Errorf("ci.required_checks[%d]: %q appears twice", i, n)
		}
		seen[n] = true
	}
	return slices.Clone(names), nil
}

// ciDuration reads a Go duration such as "1m", or def when s is empty.
func ciDuration(key, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive duration such as 1m", key, s)
	}
	return d, nil
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
	openers, err := compileMarkers("tests.comment_openers", t.CommentOpeners)
	if err != nil {
		return TestPolicy{}, err
	}
	closers, err := compileMarkers("tests.comment_closers", t.CommentClosers)
	if err != nil {
		return TestPolicy{}, err
	}
	if (len(openers) == 0) != (len(closers) == 0) {
		return TestPolicy{}, errors.New("tests.comment_openers and tests.comment_closers: set both or neither")
	}
	return TestPolicy{
		Paths: t.Paths, CaseMarkers: cases, SkipMarkers: skips,
		CommentOpeners: openers, CommentClosers: closers, ConfigPaths: t.ConfigPaths,
	}, nil
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
