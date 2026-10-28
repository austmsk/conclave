package agent

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/deployfile"
)

// ErrInvalidToolchains wraps every toolchains.yaml validation failure.
var ErrInvalidToolchains = errors.New("invalid toolchains configuration")

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
	if err := deployfile.Decode(r, &f, ErrInvalidToolchains); err != nil {
		return nil, fmt.Errorf("toolchains configuration: %w", err)
	}
	if len(f.Repositories) == 0 {
		return nil, fmt.Errorf("%w: no repositories configured", ErrInvalidToolchains)
	}
	names, err := deployfile.RepoKeys(f.Repositories)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToolchains, err)
	}
	out := make(map[string]Toolchain, len(f.Repositories))
	for _, name := range names {
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
