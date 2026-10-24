package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Environment variables for the real-binary tests. CI downloads the pinned
// release, checks it, and sets all three (see .github/workflows/ci.yml).
const (
	envGitleaksPath     = "CONCLAVE_GITLEAKS_PATH"
	envGitleaksSHA256   = "CONCLAVE_GITLEAKS_SHA256"
	envGitleaksRequired = "CONCLAVE_GITLEAKS_REQUIRED"
)

// platformConfig is the config the worker ships.
const platformConfig = "../../deploy/gitleaks.toml"

// fakeBehaviour scripts the stand-in gitleaks.
type fakeBehaviour struct {
	exit     int
	report   string // written to --report-path unless noReport
	noReport bool
	hang     bool // sleep past any test timeout instead of exiting
}

// fakeGitleaks is a shell script standing in for gitleaks, pinned by its
// own SHA-256. It records its arguments, environment, working directory
// and that directory's listing, and its stdin, under out; then writes the
// scripted report to the --report-path argument and exits with the
// scripted code. The process gets an empty environment, so every command
// is named by absolute path and everything else is baked in.
type fakeGitleaks struct {
	pin    GitleaksPin
	config string
	out    string
}

func newFakeGitleaks(t *testing.T, b fakeBehaviour) fakeGitleaks {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	var s strings.Builder
	s.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&s, "out=%s\n", shellQuote(out))
	s.WriteString(`printf '%s\n' "$@" > "$out/args"
/usr/bin/env > "$out/env"
pwd -P > "$out/pwd"
/bin/ls -A > "$out/ls"
/bin/cat > "$out/stdin"
report=
while [ $# -gt 0 ]; do
  if [ "$1" = --report-path ]; then report=$2; fi
  shift
done
`)
	if b.hang {
		s.WriteString("exec /bin/sleep 30\n")
	}
	if !b.noReport {
		fmt.Fprintf(&s, "printf '%%s' %s > \"$report\"\n", shellQuote(b.report))
	}
	fmt.Fprintf(&s, "exit %d\n", b.exit)

	bin := filepath.Join(dir, "gitleaks")
	if err := os.WriteFile(bin, []byte(s.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "gitleaks.toml")
	if err := os.WriteFile(config, []byte("title = \"fake\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fakeGitleaks{pin: GitleaksPin{Path: bin, Version: "fake", SHA256: fileSHA256(t, bin)}, config: config, out: out}
}

// start returns a Gitleaks over the fake, closed when the test ends.
func (f fakeGitleaks) start(t *testing.T) *Gitleaks {
	t.Helper()
	g, err := NewGitleaks(f.pin, f.config)
	if err != nil {
		t.Fatalf("NewGitleaks: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// recorded reads one file the fake wrote.
func (f fakeGitleaks) recorded(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.out, name))
	if err != nil {
		t.Fatalf("reading what the fake recorded: %v", err)
	}
	return string(b)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const oneFinding = `[{"RuleID":"conclave-canary","StartLine":2,"Match":"REDACTED","Secret":"REDACTED","File":""}]`

func TestGitleaksOutcomes(t *testing.T) {
	cases := []struct {
		name string
		b    fakeBehaviour
		want []Finding
	}{
		{"exit 0 with an empty report is no findings", fakeBehaviour{exit: 0, report: "[]"}, []Finding{}},
		{"exit 99 with findings", fakeBehaviour{exit: 99, report: oneFinding}, []Finding{{RuleID: "conclave-canary", Line: 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitleaks(t, tc.b)
			got, err := f.start(t).Scan(context.Background(), []byte("a\nb\n"))
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("findings = %+v, want %+v", got, tc.want)
			}
			if in := f.recorded(t, "stdin"); in != "a\nb\n" {
				t.Errorf("gitleaks read %q on stdin, want the input", in)
			}
		})
	}
}

// TestSecretsExitMismatch: the exit code and the report must agree, and
// the report must be one gitleaks would write. Anything else errors the
// attempt rather than deciding it.
func TestSecretsExitMismatch(t *testing.T) {
	cases := []struct {
		name string
		b    fakeBehaviour
	}{
		{"exit 0 with findings", fakeBehaviour{exit: 0, report: oneFinding}},
		{"exit 99 with none", fakeBehaviour{exit: 99, report: "[]"}},
		{"exit 1, gitleaks' error code", fakeBehaviour{exit: 1, report: "[]"}},
		{"exit 126, unknown flag", fakeBehaviour{exit: 126, noReport: true}},
		{"exit 0 without a report", fakeBehaviour{exit: 0, noReport: true}},
		{"exit 99 without a report", fakeBehaviour{exit: 99, noReport: true}},
		{"unparseable report", fakeBehaviour{exit: 99, report: `[{"RuleID":`}},
		{"null report", fakeBehaviour{exit: 0, report: "null"}},
		{"object report", fakeBehaviour{exit: 99, report: `{"RuleID":"r","StartLine":1}`}},
		{"rule ID that is not one", fakeBehaviour{exit: 99, report: `[{"RuleID":"a b\n","StartLine":1}]`}},
		{"no line", fakeBehaviour{exit: 99, report: `[{"RuleID":"r","StartLine":0}]`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitleaks(t, tc.b).start(t)
			got, err := g.Scan(context.Background(), []byte("a\n"))
			if !errors.Is(err, ErrEnvironment) {
				t.Fatalf("Scan = %+v, %v; want ErrEnvironment", got, err)
			}
		})
	}
}

func TestGitleaksTimeout(t *testing.T) {
	g := newFakeGitleaks(t, fakeBehaviour{hang: true}).start(t)
	g.timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := g.Scan(context.Background(), []byte("a\n"))
	if !errors.Is(err, ErrEnvironment) {
		t.Fatalf("err = %v, want ErrEnvironment", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a hung scanner held the gate for %s", took)
	}
}

// TestSecretsMissing: no binary, no config, or a copy that disappeared
// all error the attempt; none is read as "no secrets".
func TestSecretsMissing(t *testing.T) {
	f := newFakeGitleaks(t, fakeBehaviour{report: "[]"})
	t.Run("binary", func(t *testing.T) {
		pin := f.pin
		pin.Path = filepath.Join(t.TempDir(), "gitleaks")
		if _, err := NewGitleaks(pin, f.config); !errors.Is(err, ErrEnvironment) {
			t.Fatalf("err = %v, want ErrEnvironment", err)
		}
	})
	t.Run("config", func(t *testing.T) {
		if _, err := NewGitleaks(f.pin, filepath.Join(t.TempDir(), "none.toml")); !errors.Is(err, ErrEnvironment) {
			t.Fatalf("err = %v, want ErrEnvironment", err)
		}
	})
	t.Run("copy removed before a scan", func(t *testing.T) {
		g := f.start(t)
		if err := os.Remove(g.exe); err != nil {
			t.Fatal(err)
		}
		_, err := checkSecrets(context.Background(), g, twoFiles)
		if !errors.Is(err, ErrEnvironment) {
			t.Fatalf("err = %v, want ErrEnvironment", err)
		}
	})
}

// TestSecretsWrongHash: a binary that is not the pinned one never runs,
// whether it was wrong at startup or swapped afterwards.
func TestSecretsWrongHash(t *testing.T) {
	f := newFakeGitleaks(t, fakeBehaviour{report: "[]"})
	pins := map[string]GitleaksPin{
		"another binary's hash": {Path: f.pin.Path, Version: "fake", SHA256: strings.Repeat("0", 64)},
		"uppercase hash":        {Path: f.pin.Path, Version: "fake", SHA256: strings.ToUpper(f.pin.SHA256)},
		"no hash":               {Path: f.pin.Path, Version: "fake"},
		"no version":            {Path: f.pin.Path, SHA256: f.pin.SHA256},
	}
	for name, pin := range pins {
		t.Run(name, func(t *testing.T) {
			if _, err := NewGitleaks(pin, f.config); !errors.Is(err, ErrEnvironment) {
				t.Fatalf("err = %v, want ErrEnvironment", err)
			}
		})
	}
	t.Run("copy swapped after startup", func(t *testing.T) {
		g := f.start(t)
		marker := filepath.Join(t.TempDir(), "ran")
		if err := os.Chmod(g.exe, 0o700); err != nil {
			t.Fatal(err)
		}
		swapped := "#!/bin/sh\n: > " + shellQuote(marker) + "\nexit 0\n"
		if err := os.WriteFile(g.exe, []byte(swapped), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := checkSecrets(context.Background(), g, twoFiles)
		if !errors.Is(err, ErrEnvironment) {
			t.Fatalf("err = %v, want ErrEnvironment", err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("the swapped binary ran")
		}
	})
}

// realGitleaks returns the pinned gitleaks named by the environment. It
// skips when none is configured, unless CONCLAVE_GITLEAKS_REQUIRED=1, as
// in CI, where a missing binary fails: the real scan must not pass by not
// running.
func realGitleaks(t *testing.T) *Gitleaks {
	t.Helper()
	path := os.Getenv(envGitleaksPath)
	if path == "" {
		if os.Getenv(envGitleaksRequired) == "1" {
			t.Fatalf("%s=1 but %s is not set", envGitleaksRequired, envGitleaksPath)
		}
		t.Skipf("no gitleaks: set %s and %s to run against the real binary", envGitleaksPath, envGitleaksSHA256)
	}
	sum := os.Getenv(envGitleaksSHA256)
	if sum == "" {
		t.Fatalf("%s is set but %s is not", envGitleaksPath, envGitleaksSHA256)
	}
	g, err := NewGitleaks(GitleaksPin{Path: path, Version: "from-environment", SHA256: sum}, platformConfig)
	if err != nil {
		t.Fatalf("NewGitleaks: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestGitleaks_Real scans through the pinned binary and the platform
// config. A planted canary is found at the right file and added line, and
// clean text passes.
func TestGitleaks_Real(t *testing.T) {
	g := realGitleaks(t)
	canary := "canary_" + "gate_real_7f3k"
	cases := []struct {
		name       string
		changes    []FileChange
		wantPassed bool
		wantDetail string
	}{
		{"clean text passes", twoFiles, true, ""},
		{"a planted canary fails", []FileChange{
			twoFiles[0],
			{Kind: ChangeAdded, NewPath: "src/b.go", Added: []string{"b1", `const fixture = "` + canary + `"`}},
		}, false, `conclave-canary at "src/b.go" added line 2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := checkSecrets(context.Background(), g, tc.changes)
			if err != nil {
				t.Fatalf("checkSecrets: %v", err)
			}
			if v.Passed != tc.wantPassed || !strings.Contains(v.Detail, tc.wantDetail) {
				t.Errorf("verdict %+v, want passed %v with detail containing %q", v, tc.wantPassed, tc.wantDetail)
			}
			if strings.Contains(v.Detail, canary) {
				t.Errorf("detail holds the canary: %q", v.Detail)
			}
		})
	}
}
