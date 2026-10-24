package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// GitleaksPin identifies the gitleaks binary the worker trusts.
type GitleaksPin struct {
	Path    string
	Version string

	// SHA256 is the lowercase hex SHA-256 of the extracted binary, not of
	// the release tarball.
	SHA256 string
}

const (
	// gitleaksFoundExit is the exit code requested for "leaks found". It
	// is not gitleaks' default of 1, which it also uses for errors, so a
	// crash can never be read as a verdict.
	gitleaksFoundExit = 99

	// defaultGitleaksTimeout bounds one scan. The input is capped by
	// MaxArtifactBytes, which gitleaks scans in well under a second.
	defaultGitleaksTimeout = 2 * time.Minute

	// maxGitleaksBinary refuses a pin pointing at something absurd before
	// reading it into memory.
	maxGitleaksBinary = 512 << 20

	// maxGitleaksReport caps the report read back. Only rule IDs and line
	// numbers are kept, so a larger report means something is wrong.
	maxGitleaksReport = 16 << 20

	// busyRetries bounds retries of a start that failed with ETXTBSY; see
	// startGitleaks.
	busyRetries = 5
)

var (
	sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// ruleIDRE is what a rule ID from the platform config looks like.
	// Anything else in a report is refused rather than written to Detail.
	ruleIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Gitleaks is the production SecretScanner: a gitleaks binary pinned by
// SHA-256, run on the worker against stdin with the platform config.
//
// NewGitleaks copies the verified binary and the config into a private
// directory, and every scan runs that copy after hashing it again. Hashing
// pin.Path and then executing pin.Path would leave a window in which the
// file could be swapped between the check and the exec; the copy lives in
// a 0700 directory only the worker's user can enter, so the check and the
// exec see the same bytes unless that user is already compromised.
type Gitleaks struct {
	pin     GitleaksPin
	dir     string
	exe     string
	config  string
	timeout time.Duration
}

var _ SecretScanner = (*Gitleaks)(nil)

// NewGitleaks verifies the binary at pin.Path against pin.SHA256 and
// copies it and the config at configPath into a private directory. A
// missing binary or a hash mismatch is ErrEnvironment: the worker must not
// gate without its scanner. Close removes the copies.
func NewGitleaks(pin GitleaksPin, configPath string) (*Gitleaks, error) {
	if pin.Path == "" || pin.Version == "" || !sha256HexRE.MatchString(pin.SHA256) {
		return nil, environmentf("gitleaks pin needs a path, a version and a lowercase hex SHA-256")
	}
	bin, err := readBinary(pin.Path)
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(bin); got != pin.SHA256 {
		return nil, environmentf("gitleaks %s at %s has SHA-256 %s, pinned %s", pin.Version, pin.Path, got, pin.SHA256)
	}
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return nil, environmentf("reading gitleaks config: %v", err)
	}
	dir, err := os.MkdirTemp("", "conclave-gitleaks-")
	if err != nil {
		return nil, environmentf("creating gitleaks directory: %v", err)
	}
	g := &Gitleaks{
		pin:     pin,
		dir:     dir,
		exe:     filepath.Join(dir, "gitleaks"),
		config:  filepath.Join(dir, "gitleaks.toml"),
		timeout: defaultGitleaksTimeout,
	}
	if err := g.install(bin, cfg); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return g, nil
}

func readBinary(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, environmentf("gitleaks binary: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxGitleaksBinary {
		return nil, environmentf("gitleaks binary %s is not a regular file under %d bytes", path, maxGitleaksBinary)
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		return nil, environmentf("reading gitleaks binary: %v", err)
	}
	return bin, nil
}

// install writes the private copies and checks the binary's hash as
// written.
func (g *Gitleaks) install(bin, cfg []byte) error {
	if err := os.WriteFile(g.exe, bin, 0o500); err != nil {
		return environmentf("copying gitleaks binary: %v", err)
	}
	if err := os.WriteFile(g.config, cfg, 0o400); err != nil {
		return environmentf("copying gitleaks config: %v", err)
	}
	return g.verify()
}

// verify re-hashes the private copy of the binary.
func (g *Gitleaks) verify() error {
	bin, err := os.ReadFile(g.exe)
	if err != nil {
		return environmentf("reading gitleaks copy: %v", err)
	}
	if got := sha256Hex(bin); got != g.pin.SHA256 {
		return environmentf("gitleaks copy has SHA-256 %s, pinned %s", got, g.pin.SHA256)
	}
	return nil
}

// Close removes the private copies.
func (g *Gitleaks) Close() error {
	if err := os.RemoveAll(g.dir); err != nil {
		return fmt.Errorf("removing gitleaks directory: %w", err)
	}
	return nil
}

// Scan runs gitleaks over input and returns its findings, each with the
// input line in Line. The process gets an empty environment, so neither
// GITLEAKS_CONFIG nor GITLEAKS_CONFIG_TOML can replace the platform
// config; a fresh empty working directory, so no .gitleaks.toml or
// .gitleaksignore is found; --ignore-gitleaks-allow, so a gitleaks:allow
// comment in the diff is scanned like any other text; and --redact. Its
// output is discarded: findings come only from the report, and only rule
// IDs and line numbers are read from it.
//
// Exit 0 with an empty report is no findings, and exit 99 with at least
// one finding is findings. Every other combination is ErrEnvironment.
func (g *Gitleaks) Scan(ctx context.Context, input []byte) ([]Finding, error) {
	if err := g.verify(); err != nil {
		return nil, err
	}
	scanDir, err := os.MkdirTemp("", "conclave-gitleaks-scan-")
	if err != nil {
		return nil, environmentf("creating gitleaks scan directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(scanDir) }()
	cwd := filepath.Join(scanDir, "cwd")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		return nil, environmentf("creating gitleaks working directory: %v", err)
	}
	report := filepath.Join(scanDir, "r.json")

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	code, err := g.run(ctx, cwd, report, input)
	if err != nil {
		return nil, err
	}
	if code != 0 && code != gitleaksFoundExit {
		return nil, environmentf("gitleaks exited %d", code)
	}
	found, err := readGitleaksReport(report)
	if err != nil {
		return nil, err
	}
	if (code == 0) != (len(found) == 0) {
		return nil, environmentf("gitleaks exited %d with %d findings in its report", code, len(found))
	}
	return found, nil
}

// run starts gitleaks and returns its exit code. Failing to start, being
// killed, or timing out is an error.
func (g *Gitleaks) run(ctx context.Context, cwd, report string, input []byte) (int, error) {
	cmd, err := g.startGitleaks(ctx, cwd, report, input)
	if err != nil {
		return 0, err
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return 0, environmentf("gitleaks did not finish: %v", ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, environmentf("running gitleaks: %v", err)
	}
	return 0, nil
}

// startGitleaks starts the private copy. Go's fork can briefly hold
// another goroutine's write descriptor for a just-written executable,
// which makes exec fail with ETXTBSY (golang/go#22315); that start is
// retried with a new Cmd.
func (g *Gitleaks) startGitleaks(ctx context.Context, cwd, report string, input []byte) (*exec.Cmd, error) {
	for attempt := 0; ; attempt++ {
		cmd := g.command(ctx, cwd, report, input)
		err := cmd.Start()
		if err == nil {
			return cmd, nil
		}
		if !errors.Is(err, syscall.ETXTBSY) || attempt == busyRetries {
			return nil, environmentf("starting gitleaks: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil, environmentf("starting gitleaks: %v", ctx.Err())
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
}

func (g *Gitleaks) command(ctx context.Context, cwd, report string, input []byte) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.exe, "stdin",
		"--config", g.config,
		"--gitleaks-ignore-path", cwd,
		"--no-banner",
		"--redact",
		"--ignore-gitleaks-allow",
		"--exit-code", fmt.Sprint(gitleaksFoundExit),
		"--report-format", "json",
		"--report-path", report,
	)
	// Empty, not nil: a nil Env inherits the worker's environment,
	// credentials included.
	cmd.Env = []string{}
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(input)
	// Nil Stdout and Stderr go to the null device: nothing gitleaks
	// prints, which could echo the input, is kept.
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// gitleaksFinding is the allowlist of report fields read. Match, Secret,
// Line and the rest are never decoded, so the matched text cannot reach a
// result even if redaction failed.
type gitleaksFinding struct {
	RuleID    string `json:"RuleID"`
	StartLine int    `json:"StartLine"`
}

// readReport reads the JSON report. A missing, oversized or malformed
// report, or a finding with an unexpected rule ID or line, is
// ErrEnvironment.
func readGitleaksReport(path string) ([]Finding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, environmentf("reading gitleaks report: %v", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxGitleaksReport+1))
	if err != nil {
		return nil, environmentf("reading gitleaks report: %v", err)
	}
	if len(data) > maxGitleaksReport {
		return nil, environmentf("gitleaks report exceeds %d bytes", maxGitleaksReport)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		return nil, environmentf("gitleaks report is not a JSON array")
	}
	var raw []gitleaksFinding
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, environmentf("parsing gitleaks report: %v", err)
	}
	found := make([]Finding, 0, len(raw))
	for i, r := range raw {
		if !ruleIDRE.MatchString(r.RuleID) || r.StartLine < 1 {
			return nil, environmentf("gitleaks report finding %d has an unexpected rule ID or line", i)
		}
		found = append(found, Finding{RuleID: r.RuleID, Line: r.StartLine})
	}
	return found, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
