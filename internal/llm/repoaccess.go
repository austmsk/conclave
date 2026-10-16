package llm

import (
	"fmt"
	"io"
)

// repoAccessFile is the shape of the owner's repository access file.
// Example:
//
//	repositories:
//	  austmsk/election-tally:
//	    tier: sensitive
//	    roles: [planner]
//	    sensitive_providers: [anthropic]
//
// Repositories are keyed by full name, which is how the owner knows them;
// the database ID is looked up per request (DirectoryRepos).
type repoAccessFile struct {
	Repositories map[string]RepoAccess `yaml:"repositories"`
}

// LoadRepoAccess reads the owner's repository access configuration and
// validates it, so a missing or unknown tier, an empty or misspelt role
// allowlist, or a misspelt provider stops startup (FR-SEC12, NFR-M7). Every
// failure wraps ErrInvalidConfig, and a policy failure also wraps its
// specific error, such as ErrUnknownTier.
func LoadRepoAccess(r io.Reader) (map[string]RepoAccess, error) {
	var f repoAccessFile
	if err := decodeStrict(r, &f); err != nil {
		return nil, fmt.Errorf("repository access configuration: %w", err)
	}
	if len(f.Repositories) == 0 {
		return nil, fmt.Errorf("%w: no repositories configured", ErrInvalidConfig)
	}
	if err := validateAccess(f.Repositories); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return f.Repositories, nil
}
