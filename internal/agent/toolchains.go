package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/austmsk/conclave/internal/contracts"
)

// ErrInvalidToolchains wraps every toolchains.yaml validation failure.
var ErrInvalidToolchains = errors.New("invalid toolchains configuration")

// placeholderRepo is the key deploy/toolchains.yaml ships with. It fails
// validation, so the worker refuses to start until the owner fills it in.
const placeholderRepo = "owner/repo"

// fullName is a GitHub owner/repo: an owner of letters, digits and
// hyphens, and a repository name of letters, digits, '.', '_' and '-'.
var fullName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// Toolchain is one repository's entry in deploy/toolchains.yaml: the
// commands its tools run and the profile its agent prompt describes.
type Toolchain struct {
	Commands Commands

	// Profile has FullName set from the repository's key. Milestone 3's
	// codebase brief replaces the owner-written fields.
	Profile contracts.RepoProfile
}

// toolchainsFile is the shape of deploy/toolchains.yaml. Example:
//
//	repositories:
//	  austmsk/election-tally:
//	    test: [go, test, ./...]
//	    build: [go, build, ./...]
//	    lint: [golangci-lint, run]
//	    profile:
//	      language: Go
//	      layout_hint: "Packages under internal/, commands under cmd/"
//	      conventions: "Table-driven tests beside the code; errors wrapped with context"
//
// Repositories are keyed by full name, as in deploy/repos.yaml.
type toolchainsFile struct {
	Repositories map[string]toolchainEntry `yaml:"repositories"`
}

// toolchainEntry is one repository as written: the commands sit beside
// the profile block, not under a key of their own.
type toolchainEntry struct {
	Commands `yaml:",inline"`

	// Profile is a pointer so that a missing block gets its own error.
	Profile *profileEntry `yaml:"profile"`
}

type profileEntry struct {
	Language    string `yaml:"language"`
	LayoutHint  string `yaml:"layout_hint"`
	Conventions string `yaml:"conventions"`
}

// LoadToolchains reads deploy/toolchains.yaml: each repository's
// configured commands and profile, keyed by full name as in
// deploy/repos.yaml. Unknown fields are an error, so a misspelt key stops
// startup instead of leaving a command silently unconfigured. Every
// command tool the Implementer role lists must be configured, because the
// runner refuses a role whose tools it cannot provide: a gap here would
// otherwise fail every attempt on that repository instead of startup. The
// profile block and its three fields are required; values are trimmed of
// surrounding whitespace, and a blank one is an error. Every failure wraps
// ErrInvalidToolchains and names the offending key.
func LoadToolchains(r io.Reader) (map[string]Toolchain, error) {
	var f toolchainsFile
	if err := decodeToolchains(r, &f); err != nil {
		return nil, err
	}
	if len(f.Repositories) == 0 {
		return nil, fmt.Errorf("%w: no repositories configured", ErrInvalidToolchains)
	}
	out := make(map[string]Toolchain, len(f.Repositories))
	seen := make(map[string]string, len(f.Repositories))
	for _, name := range slices.Sorted(maps.Keys(f.Repositories)) {
		if err := validateRepoName(name); err != nil {
			return nil, fmt.Errorf("%w: repositories.%q: %w", ErrInvalidToolchains, name, err)
		}
		// GitHub names are case-insensitive, so two keys that differ only
		// in case would be one repository with two configurations.
		if other, dup := seen[strings.ToLower(name)]; dup {
			return nil, fmt.Errorf("%w: repositories.%q: same repository as %q", ErrInvalidToolchains, name, other)
		}
		seen[strings.ToLower(name)] = name
		tc, err := toolchainFor(name, f.Repositories[name])
		if err != nil {
			return nil, fmt.Errorf("%w: repositories.%q.%w", ErrInvalidToolchains, name, err)
		}
		out[name] = tc
	}
	return out, nil
}

// toolchainFor validates one repository's entry. Its errors start with
// the key inside the entry, for the caller to prefix with the repository.
func toolchainFor(name string, e toolchainEntry) (Toolchain, error) {
	if err := validateCommands(e.Commands, Implementer); err != nil {
		return Toolchain{}, err
	}
	profile, err := validateProfile(name, e.Profile)
	if err != nil {
		return Toolchain{}, err
	}
	return Toolchain{Commands: e.Commands, Profile: profile}, nil
}

// validateProfile requires the profile block and a non-blank value for
// each of its fields, and returns the trimmed profile with FullName set
// to name.
func validateProfile(name string, p *profileEntry) (contracts.RepoProfile, error) {
	if p == nil {
		return contracts.RepoProfile{}, errors.New("profile: missing; add a profile block with language, layout_hint and conventions")
	}
	profile := contracts.RepoProfile{
		FullName:    name,
		Language:    strings.TrimSpace(p.Language),
		LayoutHint:  strings.TrimSpace(p.LayoutHint),
		Conventions: strings.TrimSpace(p.Conventions),
	}
	fields := []struct{ key, value string }{
		{"language", profile.Language},
		{"layout_hint", profile.LayoutHint},
		{"conventions", profile.Conventions},
	}
	for _, f := range fields {
		if f.value == "" {
			return contracts.RepoProfile{}, fmt.Errorf("profile.%s: missing or blank; the agent prompt renders it", f.key)
		}
	}
	return profile, nil
}

// decodeToolchains decodes exactly one YAML document, rejecting unknown
// keys as llm's configuration loader does.
func decodeToolchains(r io.Reader, v any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("toolchains configuration: reading: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: parsing YAML: %w", ErrInvalidToolchains, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: more than one YAML document", ErrInvalidToolchains)
	}
	return nil
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

// validateCommands requires at least one command, and one for every
// command tool role lists. A key that is present must hold a usable argv.
func validateCommands(c Commands, role contracts.RoleSpec) error {
	cmds := []struct {
		key  string
		tool contracts.ToolName
		argv []string
	}{{"test", contracts.ToolRunTests, c.Test}, {"build", contracts.ToolRunBuild, c.Build}, {"lint", contracts.ToolRunLint, c.Lint}}
	configured := 0
	for _, cmd := range cmds {
		if cmd.argv == nil {
			continue
		}
		if err := validateArgv(cmd.argv); err != nil {
			return fmt.Errorf("%s: %w", cmd.key, err)
		}
		configured++
	}
	if configured == 0 {
		return errors.New("test/build/lint: no commands; configure them, or remove the repository")
	}
	for _, cmd := range cmds {
		if cmd.argv == nil && !role.ReadOnly && role.Allows(cmd.tool) {
			return fmt.Errorf("%s: missing, but role %s offers %s; configure it (a repository with nothing to run can use [\"true\"])",
				cmd.key, role.Role, cmd.tool)
		}
	}
	return nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 {
		return errors.New(`empty command; use ["true"] if the repository has nothing to run`)
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
