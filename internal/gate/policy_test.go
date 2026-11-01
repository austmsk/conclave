package gate

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// w1TestRepo is the repository testPolicy configures.
const w1TestRepo = "austmsk/ets"

// w1ConfigYAML is a valid gates.yaml with ETS-like values.
const w1ConfigYAML = `gitleaks:
  path: /usr/local/bin/gitleaks
  version: 8.28.0
  sha256: 1111111111111111111111111111111111111111111111111111111111111111
repositories:
  austmsk/ets:
    limits:
      max_changed_lines: 400
      max_changed_files: 20
      max_diff_bytes: 262144
    tests:
      paths: [src/*Test.java]
      case_markers: ['@(Test|ParameterizedTest|RepeatedTest|TestFactory)\b']
      skip_markers: ['@Disabled', '@DisabledIf', 'assume(True|False|That)']
      comment_openers: ['/\*([^*]|\*+[^*/])*\**$']
      comment_closers: ['^([^/]|/[^*])*\*/']
      config_paths: [lib/]
    ci:
      required_checks: [build, test]
`

// testPolicy returns a valid ETS-like policy loaded through LoadConfig:
// tests in src/*Test.java, JUnit markers, lib/ as test configuration, and
// limits of 400 lines, 20 files and 256 KiB.
func testPolicy(t testing.TB) Policy {
	t.Helper()
	cfg, err := LoadConfig(strings.NewReader(w1ConfigYAML))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	p, ok := cfg.Repositories[w1TestRepo]
	if !ok {
		t.Fatalf("no policy for %s", w1TestRepo)
	}
	return p
}

func TestLoadConfig_Valid(t *testing.T) {
	p := testPolicy(t)
	want := SizeLimits{MaxChangedLines: 400, MaxChangedFiles: 20, MaxDiffBytes: 262144}
	if p.Limits != want {
		t.Errorf("limits %+v, want %+v", p.Limits, want)
	}
	if p.FlagRejectionsAt != defaultFlagRejectionsAt {
		t.Errorf("flag_rejections_at %d, want default %d", p.FlagRejectionsAt, defaultFlagRejectionsAt)
	}
	if p.Mode != "" {
		t.Errorf("mode %q, want empty", p.Mode)
	}
	if len(p.Tests.CaseMarkers) != 1 || len(p.Tests.SkipMarkers) != 3 {
		t.Errorf("markers %d/%d, want 1/3", len(p.Tests.CaseMarkers), len(p.Tests.SkipMarkers))
	}
	if p.compiled == nil {
		t.Error("patterns not compiled at load")
	}
	wantCI := CIPolicy{RequiredChecks: []string{"build", "test"}, CheckApp: "github-actions", PollEvery: time.Minute, Timeout: time.Hour}
	if !reflect.DeepEqual(p.CI, wantCI) {
		t.Errorf("ci %+v, want the defaults %+v", p.CI, wantCI)
	}
	cfg, err := LoadConfig(strings.NewReader(w1ConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	wantPin := GitleaksPin{Path: "/usr/local/bin/gitleaks", Version: "8.28.0", SHA256: strings.Repeat("1", 64)}
	if cfg.Gitleaks != wantPin {
		t.Errorf("gitleaks %+v, want %+v", cfg.Gitleaks, wantPin)
	}
}

func TestLoadConfig_Optional(t *testing.T) {
	yml := strings.Replace(w1ConfigYAML, "    limits:\n",
		"    mode: clean_room\n    flag_rejections_at: 5\n    protected_paths: [secrets/]\n    execution_surface_paths: [scripts/]\n    limits:\n", 1)
	cfg, err := LoadConfig(strings.NewReader(yml))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Repositories[w1TestRepo]
	if p.Mode != contracts.GatingCleanRoom || p.FlagRejectionsAt != 5 {
		t.Fatalf("mode %q, flag_rejections_at %d", p.Mode, p.FlagRejectionsAt)
	}
	c := p.Constraints()
	if p.CI.CheckApp != "github-actions" {
		t.Errorf("check_app %q", p.CI.CheckApp)
	}
	if !slices.Contains(c.ProtectedPaths, "secrets/") || !slices.Contains(c.ProtectedPaths, ".github/") {
		t.Errorf("protected paths %v lack the addition or a default", c.ProtectedPaths)
	}
	if !slices.Contains(c.ExecutionSurfacePaths, "scripts/") || !slices.Contains(c.ExecutionSurfacePaths, ".claude/") {
		t.Errorf("surface paths %v lack the addition or a default", c.ExecutionSurfacePaths)
	}
}

func TestLoadConfig_Invalid(t *testing.T) {
	repl := func(old, new string) string {
		if !strings.Contains(w1ConfigYAML, old) {
			panic("test fixture does not contain " + old)
		}
		return strings.Replace(w1ConfigYAML, old, new, 1)
	}
	cases := []struct{ name, yaml, errPart string }{
		{"placeholder", repl("austmsk/ets:", "owner/repo:"), `repositories."owner/repo": placeholder`},
		{"bad repo name", repl("austmsk/ets:", "not-a-name:"), `repositories."not-a-name"`},
		{"unknown key", repl("    limits:\n", "    limit:\n      x: 1\n    limits:\n"), "parsing YAML"},
		{"two documents", w1ConfigYAML + "---\nrepositories: {}\n", "more than one YAML document"},
		{"unknown top-level key", w1ConfigYAML + "extra: 1\n", "parsing YAML"},
		{"empty repositories", w1ConfigYAML[:strings.Index(w1ConfigYAML, "repositories:")] + "repositories: {}\n", "repositories: none configured"},
		{"gitleaks placeholder hash", repl(strings.Repeat("1", 64), strings.Repeat("0", 64)), "gitleaks.sha256: placeholder"},
		{"gitleaks missing", w1ConfigYAML[strings.Index(w1ConfigYAML, "repositories:"):], "gitleaks.path/version/sha256: missing"},
		{"gitleaks relative path", repl("/usr/local/bin/gitleaks", "bin/gitleaks"), "gitleaks.path"},
		{"gitleaks unclean path", repl("/usr/local/bin/gitleaks", "/usr/local/../bin/gitleaks"), "gitleaks.path"},
		{"gitleaks no version", repl("version: 8.28.0", "version: ''"), "gitleaks.version"},
		{"gitleaks short hash", repl(strings.Repeat("1", 64), strings.Repeat("1", 63)), "gitleaks.sha256"},
		{"gitleaks uppercase hash", repl(strings.Repeat("1", 64), strings.Repeat("A", 64)), "gitleaks.sha256"},
		{"unknown mode", repl("    limits:\n", "    mode: yolo\n    limits:\n"), `repositories."austmsk/ets".mode`},
		{"limits missing", repl("    limits:\n      max_changed_lines: 400\n      max_changed_files: 20\n      max_diff_bytes: 262144\n", ""), ".limits: required"},
		{"zero lines", repl("max_changed_lines: 400", "max_changed_lines: 0"), "limits.max_changed_lines"},
		{"zero files", repl("max_changed_files: 20", "max_changed_files: 0"), "limits.max_changed_files"},
		{"negative bytes", repl("max_diff_bytes: 262144", "max_diff_bytes: -1"), "limits.max_diff_bytes"},
		{"tests missing", repl("    tests:\n      paths: [src/*Test.java]\n", "    unused_tests:\n      paths: [src/*Test.java]\n"), "parsing YAML"},
		{"tests paths empty", repl("paths: [src/*Test.java]", "paths: []"), "tests.paths: required"},
		{"bad case marker", repl(`'@(Test|ParameterizedTest|RepeatedTest|TestFactory)\b'`, `'@(Test'`), "tests.case_markers[0]"},
		{"empty skip marker", repl(`'@Disabled', `, `'', `), "tests.skip_markers[0]: empty"},
		{"bad comment closer", repl(`'^([^/]|/[^*])*\*/'`, `'(['`), "tests.comment_closers[0]"},
		{"comment openers without closers", repl("      comment_closers: ['^([^/]|/[^*])*\\*/']\n", ""), "set both or neither"},
		{"marker matches empty", repl(`'@Disabled', `, `'x*', `), "tests.skip_markers[0]: matches an empty line"},
		{"double star pattern", repl("paths: [src/*Test.java]", "paths: ['src/**/*Test.java']"), "tests.paths"},
		{"absolute config path", repl("config_paths: [lib/]", "config_paths: [/lib/]"), "tests.config_paths"},
		{"bad protected path", repl("    limits:\n", "    protected_paths: ['a/../b']\n    limits:\n"), "protected_paths"},
		{"bad surface path", repl("    limits:\n", "    execution_surface_paths: ['[']\n    limits:\n"), "execution_surface_paths"},
		{"zero flag rejections", repl("    limits:\n", "    flag_rejections_at: 0\n    limits:\n"), "flag_rejections_at"},
		{"case duplicate", w1ConfigYAML + strings.Replace(w1ConfigYAML[strings.Index(w1ConfigYAML, "  austmsk/ets:"):], "austmsk/ets", "Austmsk/ETS", 1), "same repository"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(strings.NewReader(tc.yaml))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("error %q does not contain %q", err, tc.errPart)
			}
		})
	}
}

// TestLoadConfig_InvalidCI refuses each malformed ci block, naming the
// key.
func TestLoadConfig_InvalidCI(t *testing.T) {
	repl := func(old, new string) string {
		if !strings.Contains(w1ConfigYAML, old) {
			panic("test fixture does not contain " + old)
		}
		return strings.Replace(w1ConfigYAML, old, new, 1)
	}
	cases := []struct{ name, yaml, errPart string }{
		{"ci missing", repl("    ci:\n      required_checks: [build, test]\n", ""), ".ci: required"},
		{"ci no checks", repl("required_checks: [build, test]", "required_checks: []"), "ci.required_checks: required"},
		{"ci blank check", repl("required_checks: [build, test]", "required_checks: [build, ' ']"), "ci.required_checks[1]: empty"},
		{"ci duplicate check", repl("required_checks: [build, test]", "required_checks: [build, build]"), "ci.required_checks[1]: \"build\" appears twice"},
		{"ci bad app", repl("required_checks: [build, test]", "required_checks: [build]\n      check_app: Not An App"), "ci.check_app"},
		{"ci bad poll", repl("required_checks: [build, test]", "required_checks: [build]\n      poll_every: often"), "ci.poll_every"},
		{"ci negative timeout", repl("required_checks: [build, test]", "required_checks: [build]\n      timeout: -1m"), "ci.timeout"},
		{"ci poll too fast", repl("required_checks: [build, test]", "required_checks: [build]\n      poll_every: 5s"), "ci.poll_every: 5s is under"},
		{"ci timeout under poll", repl("required_checks: [build, test]", "required_checks: [build]\n      poll_every: 2m\n      timeout: 1m"), "ci.timeout: 1m0s is shorter"},
		{"ci timeout too long", repl("required_checks: [build, test]", "required_checks: [build]\n      timeout: 25h"), "ci.timeout: 25h0m0s is over"},
		{"ci unknown key", repl("required_checks: [build, test]", "required_checks: [build]\n      required: true"), "parsing YAML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(strings.NewReader(tc.yaml))
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("got %v, want ErrInvalidConfig naming %q", err, tc.errPart)
			}
		})
	}
}

// TestLoadConfig_CISettings reads every ci setting the owner may give.
func TestLoadConfig_CISettings(t *testing.T) {
	yml := strings.Replace(w1ConfigYAML, "required_checks: [build, test]",
		"required_checks: [verify]\n      check_app: buildkite\n      poll_every: 30s\n      timeout: 2h", 1)
	cfg, err := LoadConfig(strings.NewReader(yml))
	if err != nil {
		t.Fatal(err)
	}
	want := CIPolicy{RequiredChecks: []string{"verify"}, CheckApp: "buildkite", PollEvery: 30 * time.Second, Timeout: 2 * time.Hour}
	if got := cfg.Repositories[w1TestRepo].CI; !reflect.DeepEqual(got, want) {
		t.Fatalf("ci %+v, want %+v", got, want)
	}
}

func TestDeployedGatesConfigIsPlaceholder(t *testing.T) {
	shipped, err := os.ReadFile("../../deploy/gates.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(strings.NewReader(string(shipped)))
	if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("got %v, want the placeholder to fail validation", err)
	}
	// With the placeholder filled in, the shipped file is otherwise valid,
	// so the comments describe a loadable shape.
	filled := strings.Replace(string(shipped), "  owner/repo:\n", "  austmsk/election-tally:\n", 1)
	filled = strings.Replace(filled, strings.Repeat("0", 64), strings.Repeat("1", 64), 1)
	if strings.Contains(filled, "owner/repo:") || strings.Contains(filled, strings.Repeat("0", 64)) {
		t.Fatal("placeholders not found in deploy/gates.yaml")
	}
	if _, err := LoadConfig(strings.NewReader(filled)); err != nil {
		t.Fatalf("filled-in deploy/gates.yaml: %v", err)
	}
}

func TestPolicyConstraints(t *testing.T) {
	p := Policy{
		Limits:                SizeLimits{MaxChangedLines: 123, MaxChangedFiles: 4, MaxDiffBytes: 5},
		ProtectedPaths:        []string{"infra/"},
		ExecutionSurfacePaths: []string{"tools/hook.sh"},
	}
	got := p.Constraints()
	want := contracts.Constraints{
		MaxChangedLines:       123,
		ProtectedPaths:        append(slices.Clone(defaultProtectedPaths), "infra/"),
		ExecutionSurfacePaths: append(slices.Clone(defaultExecutionSurfacePaths), "tools/hook.sh"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// The defaults are a floor shared by every policy; one policy's
	// constraints must not be able to change them.
	got.ProtectedPaths[0] = "changed"
	if defaultProtectedPaths[0] == "changed" {
		t.Fatal("Constraints aliases the defaults")
	}
}

// TestPolicyPatternsFollowEdits checks that compiled patterns never go
// stale: a policy edited after LoadConfig is matched by its new lists.
func TestPolicyPatternsFollowEdits(t *testing.T) {
	p := testPolicy(t)
	p.ProtectedPaths = []string{"infra/"}
	change := []FileChange{{Kind: ChangeModified, OldPath: "infra/main.tf", NewPath: "infra/main.tf", Added: []string{"x"}}}
	if v := checkProtected(change, p); v.Passed {
		t.Fatalf("edited policy ignored: %+v", v)
	}
	p = testPolicy(t)
	p.Tests.Paths = []string{"test/"}
	del := []FileChange{{Kind: ChangeDeleted, OldPath: "test/a_test.py"}}
	if v := checkWeakening(del, p); v.Passed {
		t.Fatalf("edited test paths ignored: %+v", v)
	}
}
