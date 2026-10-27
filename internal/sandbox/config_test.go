package sandbox

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// realDigest is well formed and not the placeholder; nothing pulls it.
const realDigest = "@sha256:1111111111111111111111111111111111111111111111111111111111111111"

const validConfig = `repositories:
  austmsk/election-tally:
    default_branch: main
    image: ghcr.io/austmsk/conclave-sandbox-java21` + realDigest + `
    limits:
      memory: 2GiB
      cpu_shares: 1024
      disk: 1GiB
      max_procs: 256
      timeout: 25m
    command_timeout: 5m
    install:
      - [sh, -c, "mkdir -p lib && cp /opt/junit/*.jar lib/"]
    env:
      JAVA_TOOL_OPTIONS: "-Duser.timezone=UTC"
      TALLY_API_KEY: canary_tally_api_key
`

func validWant() map[string]RepoSandbox {
	return map[string]RepoSandbox{"austmsk/election-tally": {
		DefaultBranch: "main",
		Image:         "ghcr.io/austmsk/conclave-sandbox-java21" + realDigest,
		Limits: contracts.ResourceLimits{
			MemoryBytes: 2 << 30,
			CPUShares:   1024,
			DiskBytes:   1 << 30,
			MaxProcs:    256,
			Timeout:     25 * time.Minute,
		},
		CommandTimeout: 5 * time.Minute,
		Install:        [][]string{{"sh", "-c", "mkdir -p lib && cp /opt/junit/*.jar lib/"}},
		Env:            map[string]string{"JAVA_TOOL_OPTIONS": "-Duser.timezone=UTC", "TALLY_API_KEY": "canary_tally_api_key"},
	}}
}

// edit returns validConfig with old replaced by new, failing loudly if old
// is not there, so a case cannot silently test the valid file.
func edit(t *testing.T, old, new string) string {
	t.Helper()
	out := strings.Replace(validConfig, old, new, 1)
	if out == validConfig {
		t.Fatalf("%q not found in validConfig", old)
	}
	return out
}

func TestLoadConfigAcceptsAValidFile(t *testing.T) {
	got, err := LoadConfig(strings.NewReader(validConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(got, validWant()) {
		t.Fatalf("got %+v, want %+v", got, validWant())
	}
}

// rejectCase edits validConfig by replacing old with new, or, when raw is
// set, loads raw instead.
type rejectCase struct {
	name, old, new string
	raw            string
	errPart        string
}

func TestLoadConfigRejects(t *testing.T) {
	const repo = `repositories."austmsk/election-tally".`
	cases := []rejectCase{
		{"placeholder repository", "austmsk/election-tally:", "owner/repo:", "", `repositories."owner/repo": placeholder`},
		{"all-zero digest", realDigest, placeholderDigest, "", repo + "image: placeholder all-zero digest"},
		{"image by tag", realDigest, ":21", "", repo + `image: "ghcr.io/austmsk/conclave-sandbox-java21:21" is not pinned by digest`},
		{"image by short digest", realDigest, "@sha256:abcd", "", repo + "image: "},
		{"image missing", "    image: ghcr.io/austmsk/conclave-sandbox-java21" + realDigest + "\n", "", "", repo + "image: missing"},
		{"zero memory", "memory: 2GiB", "memory: 0GiB", "", repo + `limits.memory: size "0GiB" must be positive`},
		{"zero cpu shares", "cpu_shares: 1024", "cpu_shares: 0", "", repo + "limits.cpu_shares: missing or not positive"},
		{"missing max procs", "      max_procs: 256\n", "", "", repo + "limits.max_procs: missing or not positive"},
		{"zero sandbox timeout", "timeout: 25m", "timeout: 0s", "", repo + `limits.timeout: duration "0s" must be positive`},
		{"size without a unit", "memory: 2GiB", "memory: 2147483648", "", repo + `limits.memory: size "2147483648" is not a whole number`},
		{"size in decimal units", "memory: 2GiB", "memory: 2GB", "", repo + `limits.memory: size "2GB" is not a whole number`},
		{"fractional size", "disk: 1GiB", "disk: 1.5GiB", "", repo + `limits.disk: size "1.5GiB" is not a whole number`},
		{"size with a space", "disk: 1GiB", "disk: 1 GiB", "", repo + `limits.disk: size "1 GiB" is not a whole number`},
		{"negative size", "disk: 1GiB", "disk: -1GiB", "", repo + `limits.disk: size "-1GiB" is not a whole number`},
		{"oversized size", "memory: 2GiB", "memory: 9999999999999GiB", "", repo + `limits.memory: size "9999999999999GiB" is too large`},
		{"disk not below memory", "disk: 1GiB", "disk: 2GiB", "", repo + "limits: DiskBytes (2147483648) must be below MemoryBytes"},
		{"tiny disk", "disk: 1GiB", "disk: 1MiB", "", repo + "limits: DiskBytes below"},
		{"too few procs", "max_procs: 256", "max_procs: 4", "", repo + "limits: MaxProcs below 8"},
		{"unparseable duration", "command_timeout: 5m", "command_timeout: five", "", repo + `command_timeout: duration "five"`},
		{"sandbox timeout shorter than command timeout", "timeout: 25m", "timeout: 4m", "", repo + "limits.timeout (4m0s) must be longer than command_timeout (5m0s)"},
		{"sandbox timeout equal to command timeout", "timeout: 25m", "timeout: 5m", "", repo + "limits.timeout (5m0s) must be longer than command_timeout (5m0s)"},
		{"reserved env name", "      TALLY_API_KEY:", "      HOME:", "", repo + "env.HOME: set by the sandbox provider"},
		{"env key not an identifier", "      TALLY_API_KEY:", "      TALLY-API-KEY:", "", repo + `env."TALLY-API-KEY": not an identifier`},
		{"empty default branch", "default_branch: main", `default_branch: ""`, "", repo + "default_branch: missing or empty"},
		{"empty install step", "      - [sh, -c,", "      - []\n      - [sh, -c,", "", repo + "install[0]: empty command"},
		{"install step with an empty program", "      - [sh, -c,", `      - ["", -c,`, "", repo + "install[0]: the first element, the program, is empty"},
		{"unknown repository field", "    default_branch:", "    branch: main\n    default_branch:", "", "field branch not found"},
		{"unknown limits field", "      cpu_shares:", "      cpus: 2\n      cpu_shares:", "", "field cpus not found"},
		{"second YAML document", "", "", validConfig + "---\nrepositories: {}\n", "more than one YAML document"},
		{"no repositories", "", "", "repositories: {}\n", "no repositories configured"},
		{"same repository in another case", "", "",
			validConfig + strings.Replace(strings.TrimPrefix(validConfig, "repositories:\n"), "austmsk/election-tally", "Austmsk/Election-Tally", 1),
			`repositories."austmsk/election-tally": same repository as "Austmsk/Election-Tally"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.raw
			if in == "" {
				in = edit(t, tc.old, tc.new)
			}
			_, err := LoadConfig(strings.NewReader(in))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("error %q does not contain %q", err, tc.errPart)
			}
		})
	}
}

// TestLoadConfigShippedFile checks deploy/sandboxes.yaml: as shipped it
// fails on the placeholder repository; with the name filled in it fails
// on the placeholder digest; with both filled in it loads the values the
// header documents.
func TestLoadConfigShippedFile(t *testing.T) {
	shipped, err := os.ReadFile("../../deploy/sandboxes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	named := strings.Replace(string(shipped), "  owner/repo:\n", "  austmsk/election-tally:\n", 1)
	if named == string(shipped) {
		t.Fatal("placeholder key not found in deploy/sandboxes.yaml")
	}
	pinned := strings.Replace(named, placeholderDigest, realDigest, 1)
	if pinned == named {
		t.Fatal("placeholder digest not found in deploy/sandboxes.yaml")
	}

	cases := []struct {
		name    string
		yaml    string
		errPart string
	}{
		{"as shipped", string(shipped), `repositories."owner/repo": placeholder`},
		{"name filled in", named, `repositories."austmsk/election-tally".image: placeholder all-zero digest`},
		{"name and digest filled in", pinned, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadConfig(strings.NewReader(tc.yaml))
			if tc.errPart != "" {
				if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("got %v, want ErrInvalidConfig containing %q", err, tc.errPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			want := RepoSandbox{
				DefaultBranch: "main",
				Image:         "ghcr.io/owner/conclave-sandbox-java21" + realDigest,
				Limits: contracts.ResourceLimits{
					MemoryBytes: 2 << 30, CPUShares: 1024, DiskBytes: 1 << 30, MaxProcs: 256, Timeout: 25 * time.Minute,
				},
				CommandTimeout: 5 * time.Minute,
				Install:        [][]string{},
				Env:            map[string]string{"JAVA_TOOL_OPTIONS": "-Duser.timezone=UTC"},
			}
			if g := got["austmsk/election-tally"]; !reflect.DeepEqual(g, want) {
				t.Fatalf("got %+v, want %+v", g, want)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1B", 1},
		{"512B", 512},
		{"4KiB", 4 << 10},
		{"8MiB", 8 << 20},
		{"2GiB", 2 << 30},
		{"0010MiB", 10 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("parseSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "GiB", "2", "2gib", "2G", "2Gi", "2TiB", "+2GiB", "2 GiB", " 2GiB", "2GiB ", "0x10B", "1e3B", "２GiB"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if got, err := parseSize(bad); err == nil {
				t.Fatalf("parseSize(%q) = %d, want an error", bad, got)
			}
		})
	}
}
