package sandbox

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/deployfile"
)

// ErrInvalidConfig wraps every sandboxes.yaml validation failure.
var ErrInvalidConfig = errors.New("invalid sandbox configuration")

// placeholderDigest is the all-zero digest the shipped file carries. It is
// well formed, so DigestPinned accepts it; LoadConfig refuses it by name.
const placeholderDigest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// RepoSandbox is one repository's sandbox configuration: what the worker
// needs, beyond the prepared tree, to build a contracts.SandboxSpec and
// run the gates.
type RepoSandbox struct {
	// DefaultBranch is the branch the working tree is cloned from.
	DefaultBranch string
	// Image is the sandbox image, pinned by digest (FR-SEC30).
	Image string
	// Limits bounds the whole sandbox; Limits.Timeout is its lifetime.
	Limits contracts.ResourceLimits
	// CommandTimeout bounds each gate command (gate.Commands.Timeout). It
	// is always shorter than Limits.Timeout.
	CommandTimeout time.Duration
	// Install is the install phase, one argv per step, run in order with
	// registry network before the agent phase (CreateWithInstall). Empty
	// means no install phase and no network at any point.
	Install [][]string
	// Env is the sandbox's allowlisted environment. Values for external
	// services are canaries, never real credentials.
	Env map[string]string
}

// configFile is the shape of deploy/sandboxes.yaml. Example:
//
//	repositories:
//	  austmsk/election-tally:
//	    default_branch: main
//	    image: ghcr.io/austmsk/conclave-sandbox-java21@sha256:<64 hex digits>
//	    limits: {memory: 2GiB, cpu_shares: 1024, disk: 1GiB, max_procs: 256, timeout: 25m}
//	    command_timeout: 5m
//	    install: []
//	    env: {JAVA_TOOL_OPTIONS: "-Duser.timezone=UTC"}
//
// Repositories are keyed by full name, as in deploy/repos.yaml.
type configFile struct {
	Repositories map[string]repoEntry `yaml:"repositories"`
}

type repoEntry struct {
	DefaultBranch  string            `yaml:"default_branch"`
	Image          string            `yaml:"image"`
	Limits         limitsEntry       `yaml:"limits"`
	CommandTimeout string            `yaml:"command_timeout"`
	Install        [][]string        `yaml:"install"`
	Env            map[string]string `yaml:"env"`
}

type limitsEntry struct {
	Memory    string `yaml:"memory"`
	CPUShares int64  `yaml:"cpu_shares"`
	Disk      string `yaml:"disk"`
	MaxProcs  int64  `yaml:"max_procs"`
	Timeout   string `yaml:"timeout"`
}

// LoadConfig reads deploy/sandboxes.yaml: each repository's sandbox image,
// limits, install phase, environment and default branch, keyed by full
// name as in deploy/repos.yaml. Unknown fields are an error, so a misspelt
// key stops startup instead of leaving a limit silently unset. Validation
// applies the rules a provider's Create applies (DigestPinned,
// CheckLimits, ValidEnvKey, ReservedEnv), so a configuration that would
// fail every attempt fails startup instead. Every failure wraps
// ErrInvalidConfig and names the offending key.
func LoadConfig(r io.Reader) (map[string]RepoSandbox, error) {
	var f configFile
	if err := deployfile.Decode(r, &f, ErrInvalidConfig); err != nil {
		return nil, fmt.Errorf("sandbox configuration: %w", err)
	}
	if len(f.Repositories) == 0 {
		return nil, fmt.Errorf("%w: no repositories configured", ErrInvalidConfig)
	}
	names, err := deployfile.RepoKeys(f.Repositories)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	out := make(map[string]RepoSandbox, len(f.Repositories))
	for _, name := range names {
		repo, err := buildRepo(f.Repositories[name])
		if err != nil {
			return nil, fmt.Errorf("%w: repositories.%q.%w", ErrInvalidConfig, name, err)
		}
		out[name] = repo
	}
	return out, nil
}

// buildRepo validates one entry and converts it. Errors start with the
// key path below the repository, for LoadConfig to prefix.
func buildRepo(e repoEntry) (RepoSandbox, error) {
	branch := strings.TrimSpace(e.DefaultBranch)
	if err := checkBranch(branch); err != nil {
		return RepoSandbox{}, fmt.Errorf("default_branch: %w", err)
	}
	if err := checkImage(e.Image); err != nil {
		return RepoSandbox{}, fmt.Errorf("image: %w", err)
	}
	limits, err := parseLimits(e.Limits)
	if err != nil {
		return RepoSandbox{}, err
	}
	cmdTimeout, err := parseDuration(e.CommandTimeout)
	if err != nil {
		return RepoSandbox{}, fmt.Errorf("command_timeout: %w", err)
	}
	if limits.Timeout <= cmdTimeout {
		return RepoSandbox{}, fmt.Errorf("limits.timeout (%s) must be longer than command_timeout (%s): the sandbox has to outlive every command run in it",
			limits.Timeout, cmdTimeout)
	}
	for i, argv := range e.Install {
		if err := checkArgv(argv); err != nil {
			return RepoSandbox{}, fmt.Errorf("install[%d]: %w", i, err)
		}
	}
	if err := checkEnv(e.Env); err != nil {
		return RepoSandbox{}, fmt.Errorf("env.%w", err)
	}
	return RepoSandbox{
		DefaultBranch:  branch,
		Image:          e.Image,
		Limits:         limits,
		CommandTimeout: cmdTimeout,
		Install:        e.Install,
		Env:            e.Env,
	}, nil
}

// checkBranch refuses a branch name git would refuse or misread. The name
// reaches git as an argument, so a leading '-' would be taken for a flag,
// and whitespace or a control character would fail every attempt instead
// of startup. The rest follows git check-ref-format.
func checkBranch(b string) error {
	switch {
	case b == "":
		return errors.New("missing or empty")
	case strings.HasPrefix(b, "-"):
		return fmt.Errorf("%q starts with '-', which git reads as a flag", b)
	case strings.Contains(b, "..") || strings.Contains(b, "@{") || strings.Contains(b, "//"):
		return fmt.Errorf("%q is not a valid git branch name", b)
	case strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock"):
		return fmt.Errorf("%q is not a valid git branch name", b)
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return fmt.Errorf("%q contains %q, which a git branch name cannot", b, r)
		}
	}
	return nil
}

func checkImage(image string) error {
	switch {
	case image == "":
		return errors.New("missing")
	case !DigestPinned(image):
		return fmt.Errorf("%q is not pinned by digest; use name@sha256:<64 hex digits>", image)
	case strings.HasSuffix(image, placeholderDigest):
		return errors.New("placeholder all-zero digest; replace it with the image's real digest")
	}
	return nil
}

// parseLimits converts the limits block and applies CheckLimits. Errors
// start with the key path, for buildRepo to prefix.
func parseLimits(e limitsEntry) (contracts.ResourceLimits, error) {
	var l contracts.ResourceLimits
	var err error
	if l.MemoryBytes, err = parseSize(e.Memory); err != nil {
		return l, fmt.Errorf("limits.memory: %w", err)
	}
	if l.DiskBytes, err = parseSize(e.Disk); err != nil {
		return l, fmt.Errorf("limits.disk: %w", err)
	}
	if l.Timeout, err = parseDuration(e.Timeout); err != nil {
		return l, fmt.Errorf("limits.timeout: %w", err)
	}
	if e.CPUShares <= 0 {
		return l, errors.New("limits.cpu_shares: missing or not positive")
	}
	if e.MaxProcs <= 0 {
		return l, errors.New("limits.max_procs: missing or not positive")
	}
	l.CPUShares, l.MaxProcs = e.CPUShares, e.MaxProcs
	if err := CheckLimits(l); err != nil {
		return l, fmt.Errorf("limits: %w", err)
	}
	return l, nil
}

// sizeUnits are the suffixes parseSize accepts, binary units first so "B"
// is tried only after the units that end in it.
var sizeUnits = []struct {
	suffix string
	bytes  int64
}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}

// parseSize reads a size such as "2GiB": a positive whole number followed
// directly by B, KiB, MiB or GiB. Nothing else is accepted (no decimal SI
// units, fractions, signs or spaces), so a typo cannot quietly turn a
// gigabyte into a byte.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("missing")
	}
	for _, u := range sizeUnits {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok || !allDigits(digits) {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		switch {
		case err != nil || n > math.MaxInt64/u.bytes:
			return 0, fmt.Errorf("size %q is too large", s)
		case n == 0:
			return 0, fmt.Errorf("size %q must be positive", s)
		}
		return n * u.bytes, nil
	}
	return 0, fmt.Errorf("size %q is not a whole number followed by B, KiB, MiB or GiB, as in 2GiB", s)
}

// allDigits reports a non-empty run of ASCII digits: no sign, space or
// fraction, which strconv.ParseInt alone would partly accept.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseDuration reads a positive Go duration such as "25m".
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("missing")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

// checkArgv requires a usable argv: a program, and no NUL byte, which
// cannot cross into an exec.
func checkArgv(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	if strings.TrimSpace(argv[0]) == "" {
		return errors.New("the first element, the program, is empty")
	}
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("element %d contains a NUL byte", i)
		}
	}
	return nil
}

// checkEnv applies ValidEnvKey and ReservedEnv in key order, so a file
// with two bad keys always reports the same one. Errors start with the
// key, for buildRepo to prefix with "env.".
func checkEnv(env map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(env)) {
		switch {
		case !ValidEnvKey(k):
			return fmt.Errorf("%q: not an identifier", k)
		case ReservedEnv(k):
			return fmt.Errorf("%s: set by the sandbox provider and cannot be overridden", k)
		case strings.ContainsRune(env[k], 0):
			return fmt.Errorf("%s: value contains a NUL byte", k)
		}
	}
	return nil
}
