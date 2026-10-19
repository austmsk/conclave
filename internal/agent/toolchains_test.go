package agent

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadToolchains(t *testing.T) {
	shipped, err := os.ReadFile("../../deploy/toolchains.yaml")
	if err != nil {
		t.Fatal(err)
	}
	filled := strings.Replace(string(shipped), "  owner/repo:\n", "  austmsk/election-tally:\n", 1)
	if filled == string(shipped) {
		t.Fatal("placeholder key not found in deploy/toolchains.yaml")
	}

	for _, tc := range toolchainsCases(string(shipped), filled) {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadToolchains(strings.NewReader(tc.yaml))
			if tc.errPart == "" {
				if err != nil {
					t.Fatalf("LoadToolchains: %v", err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalidToolchains) {
				t.Fatalf("got %v, want ErrInvalidToolchains", err)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("error %q does not contain %q", err, tc.errPart)
			}
		})
	}
}

type toolchainsCase struct {
	name string
	yaml string
	want map[string]Commands
	// errPart is a substring of the error; empty means it loads.
	errPart string
}

// toolchainsCases takes the shipped deploy/toolchains.yaml and the same
// file with its placeholder filled in.
func toolchainsCases(shipped, filled string) []toolchainsCase {
	return []toolchainsCase{
		{
			name: "valid",
			yaml: "repositories:\n  austmsk/election-tally:\n    test: [go, test, ./...]\n    build: [go, build, ./...]\n    lint: [sh, -c, \"golangci-lint run\"]\n",
			want: map[string]Commands{"austmsk/election-tally": {
				Test: []string{"go", "test", "./..."}, Build: []string{"go", "build", "./..."}, Lint: []string{"sh", "-c", "golangci-lint run"},
			}},
		},
		{
			name: "no linter configured as true",
			yaml: "repositories:\n  a/b:\n    test: [make, test]\n    build: [make]\n    lint: [\"true\"]\n",
			want: map[string]Commands{"a/b": {Test: []string{"make", "test"}, Build: []string{"make"}, Lint: []string{"true"}}},
		},
		// The implementer role offers all three tools, and the runner
		// refuses a role it cannot equip, so a gap fails here at startup.
		{name: "lint missing", yaml: "repositories:\n  a/b:\n    test: [make, test]\n    build: [make]\n",
			errPart: `repositories."a/b".lint: missing, but role implementer offers run_lint`},
		{name: "build missing", yaml: "repositories:\n  a/b:\n    test: [make, test]\n    lint: [make, lint]\n",
			errPart: `repositories."a/b".build: missing, but role implementer offers run_build`},
		{name: "placeholder fails validation", yaml: shipped, errPart: `repositories."owner/repo": placeholder`},
		{
			name: "placeholder filled in loads",
			yaml: filled,
			want: map[string]Commands{"austmsk/election-tally": {
				Test: []string{"go", "test", "./..."}, Build: []string{"go", "build", "./..."}, Lint: []string{"golangci-lint", "run"},
			}},
		},
		{name: "unknown top-level field", yaml: "repos:\n  a/b:\n    test: [make]\n", errPart: "field repos not found"},
		{name: "unknown command field", yaml: "repositories:\n  a/b:\n    tests: [make]\n", errPart: "field tests not found"},
		{name: "command as a string", yaml: "repositories:\n  a/b:\n    test: make test\n", errPart: "parsing YAML"},
		{name: "no repositories", yaml: "repositories: {}\n", errPart: "no repositories"},
		{name: "empty file", yaml: "", errPart: "parsing YAML"},
		{name: "two documents", yaml: "repositories:\n  a/b:\n    test: [make]\n---\nrepositories: {}\n", errPart: "more than one YAML document"},
		{name: "duplicate key", yaml: "repositories:\n  a/b:\n    test: [make]\n  a/b:\n    test: [make]\n", errPart: "already defined"},
		{name: "same repository in another case", yaml: "repositories:\n  a/b:\n    test: [make]\n  A/B:\n    test: [make]\n    build: [make]\n    lint: [make]\n",
			errPart: `repositories."a/b": same repository as "A/B"`},
		{name: "name without owner", yaml: "repositories:\n  election-tally:\n    test: [make]\n", errPart: `repositories."election-tally": not a repository full name`},
		{name: "name with two slashes", yaml: "repositories:\n  a/b/c:\n    test: [make]\n", errPart: `repositories."a/b/c": not a repository full name`},
		{name: "name with a dot-dot repo", yaml: "repositories:\n  a/..:\n    test: [make]\n", errPart: `repositories."a/..": not a valid repository name`},
		{name: "name with a space", yaml: "repositories:\n  \"a/b c\":\n    test: [make]\n", errPart: `repositories."a/b c": not a repository full name`},
		{name: "no commands", yaml: "repositories:\n  a/b: {}\n", errPart: `repositories."a/b".test/build/lint: no commands`},
		{name: "null commands", yaml: "repositories:\n  a/b:\n    test:\n", errPart: `repositories."a/b".test/build/lint: no commands`},
		{name: "empty argv", yaml: "repositories:\n  a/b:\n    test: [make]\n    lint: []\n", errPart: `repositories."a/b".lint: empty command`},
		{name: "empty program", yaml: "repositories:\n  a/b:\n    build: [\"\", build]\n", errPart: `repositories."a/b".build: the first element`},
		{name: "blank program", yaml: "repositories:\n  a/b:\n    build: [\" \"]\n", errPart: `repositories."a/b".build: the first element`},
	}
}
