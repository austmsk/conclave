package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/austmsk/conclave/internal/contracts"
)

// Provider names. They are the keys adapters are registered under and the
// values the tier table below is written against.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderGoogle    = "google"
	ProviderOllama    = "ollama"
)

// knownProviders is every provider name configuration may refer to.
var knownProviders = []string{ProviderAnthropic, ProviderOpenAI, ProviderGoogle, ProviderOllama}

// privateProviders are the hosted providers allowed for the private tier
// (security-and-threat-model.md, Sensitivity Tiers). The table is code, not
// configuration, so a configuration change cannot widen the public,
// private, or local-only tiers. The sensitive tier is the exception: its
// list is per-repository configuration by definition, bounded by
// knownProviders.
var privateProviders = []string{ProviderAnthropic, ProviderOpenAI, ProviderGoogle}

// localProviders serve models on the owner's machines.
var localProviders = []string{ProviderOllama}

// Policy errors. All of them are returned before any provider is called.
var (
	ErrUnknownRepo      = errors.New("repository is not configured")
	ErrNoTier           = errors.New("repository has no sensitivity tier")
	ErrUnknownTier      = errors.New("repository has an unknown sensitivity tier")
	ErrNoRoleAllowlist  = errors.New("repository has no role allowlist")
	ErrRoleDenied       = errors.New("role is not allowed on this repository")
	ErrTierDenied       = errors.New("provider is not allowed for the repository's tier")
	ErrUnknownProvider  = errors.New("repository policy names an unknown provider")
	ErrTierMismatch     = errors.New("repository's recorded tier differs from its configured tier")
	ErrNoEmbeddingModel = errors.New("no embedding model is configured")
)

// RepoPolicy is what the router needs to know about a repository. It comes
// from the platform repository's configuration, never from the target
// repository (FR-SEC12, FR-SEC32).
type RepoPolicy struct {
	Tier contracts.SensitivityTier

	// Roles is the repository's role allowlist. A role absent from it is
	// denied, so a new role is granted nothing by default.
	Roles []contracts.Role

	// SensitiveProviders is the owner-approved provider list for a
	// sensitive repository. It is ignored for every other tier.
	SensitiveProviders []string
}

// Repos looks up a repository's policy by ID. It is the boundary to wherever
// repository configuration is loaded from.
type Repos interface {
	Policy(ctx context.Context, repoID string) (RepoPolicy, error)

	// Validate checks what can be checked before the first request. New
	// calls it, so a misconfigured repository stops the router starting.
	// Policy still validates every policy it returns.
	Validate() error
}

// StaticRepos is a Repos backed by a map, built once at startup, keyed by
// repository ID. It suits tests; production uses DirectoryRepos, which also
// checks each repository against its database record.
type StaticRepos map[string]RepoPolicy

// Validate checks every policy, in sorted order so the error is stable.
func (s StaticRepos) Validate() error {
	for _, id := range sortedKeys(s) {
		if id == "" {
			return fmt.Errorf("repository with an empty ID: %w", ErrUnknownRepo)
		}
		if err := s[id].validate(); err != nil {
			return fmt.Errorf("repository %q: %w", id, err)
		}
	}
	return nil
}

// RepoDirectory looks up a repository's record by ID. Repo reports a
// repository that does not exist with found false and a nil error, so the
// router can tell it, which no retry fixes, from a failed lookup.
type RepoDirectory interface {
	Repo(ctx context.Context, repoID string) (repo contracts.Repo, found bool, err error)
}

// RepoAccess is what the owner's platform configuration sets for a
// repository: its tier, its role allowlist and, when it is sensitive, the
// approved providers. The configured tier is the authoritative one
// (FR-SEC12): only the owner changes it.
type RepoAccess struct {
	Tier               contracts.SensitivityTier
	Roles              []contracts.Role
	SensitiveProviders []string
}

// DirectoryRepos is the production Repos. Requests name a repository by ID,
// so each one reads the repository's record from the directory to find its
// full name, and takes the policy from configuration under that name. The
// record's tier must equal the configured one. The application's database
// role can write the repos table, so a row is not trusted to say what a
// repository's tier is; a row that disagrees with the owner's
// configuration, through tampering or drift, stops every request for that
// repository with ErrTierMismatch instead of widening its tier.
type DirectoryRepos struct {
	dir    RepoDirectory
	access map[string]RepoAccess
}

// NewDirectoryRepos keeps a copy of access, so later changes by the caller
// cannot undo Validate.
func NewDirectoryRepos(dir RepoDirectory, access map[string]RepoAccess) *DirectoryRepos {
	return &DirectoryRepos{dir: dir, access: maps.Clone(access)}
}

// Validate checks every repository's access, in sorted order so the error
// is stable. Tiers are checked per request, when they are read.
func (d *DirectoryRepos) Validate() error {
	for _, name := range sortedKeys(d.access) {
		if name == "" {
			return fmt.Errorf("access for a repository with an empty name: %w", ErrUnknownRepo)
		}
		if err := d.access[name].policy().validate(); err != nil {
			return fmt.Errorf("repository %q: %w", name, err)
		}
	}
	return nil
}

// Policy returns the configured policy for the repository with repoID,
// after checking its record agrees on the tier.
func (d *DirectoryRepos) Policy(ctx context.Context, repoID string) (RepoPolicy, error) {
	repo, found, err := d.dir.Repo(ctx, repoID)
	if err != nil {
		return RepoPolicy{}, fmt.Errorf("directory lookup: %w", err)
	}
	if !found {
		return RepoPolicy{}, fmt.Errorf("not in the directory: %w", ErrUnknownRepo)
	}
	access, ok := d.access[repo.FullName]
	if !ok {
		return RepoPolicy{}, fmt.Errorf("%q has no configured policy: %w", repo.FullName, ErrUnknownRepo)
	}
	if repo.Tier != access.Tier {
		return RepoPolicy{}, fmt.Errorf("%q is recorded as %q but configured as %q: %w",
			repo.FullName, repo.Tier, access.Tier, ErrTierMismatch)
	}
	return access.policy(), nil
}

// policy relies on RepoAccess and RepoPolicy having the same fields, so
// adding one to either without the other fails to compile.
func (a RepoAccess) policy() RepoPolicy {
	return RepoPolicy(a)
}

// Policy returns the policy for repoID, or ErrUnknownRepo.
func (s StaticRepos) Policy(_ context.Context, repoID string) (RepoPolicy, error) {
	p, ok := s[repoID]
	if !ok {
		return RepoPolicy{}, fmt.Errorf("repository %q: %w", repoID, ErrUnknownRepo)
	}
	return p, nil
}

// validate fails closed: a repository with no tier or no role allowlist
// cannot run anything, rather than defaulting to something permissive. A
// misspelt sensitive provider would deny rather than permit, but it is still
// reported, so the owner learns of it from the error and not from a refusal.
func (p RepoPolicy) validate() error {
	switch p.Tier {
	case "":
		return ErrNoTier
	case contracts.TierPublic, contracts.TierPrivate, contracts.TierSensitive, contracts.TierLocalOnly:
	default:
		return fmt.Errorf("tier %q: %w", p.Tier, ErrUnknownTier)
	}
	if len(p.Roles) == 0 {
		return ErrNoRoleAllowlist
	}
	for _, name := range p.SensitiveProviders {
		if !slices.Contains(knownProviders, name) {
			return fmt.Errorf("sensitive provider %q: %w", name, ErrUnknownProvider)
		}
	}
	return nil
}

// allowsRole reports whether role is on the repository's allowlist.
func (p RepoPolicy) allowsRole(role contracts.Role) bool {
	return slices.Contains(p.Roles, role)
}

// allowsProvider reports whether the repository's tier permits sending its
// content to provider. Unknown tiers and providers are denied.
func (p RepoPolicy) allowsProvider(provider string) bool {
	if !slices.Contains(knownProviders, provider) {
		return false
	}
	switch p.Tier {
	case contracts.TierPublic:
		return true
	case contracts.TierPrivate:
		return slices.Contains(privateProviders, provider)
	case contracts.TierSensitive:
		return slices.Contains(p.SensitiveProviders, provider)
	case contracts.TierLocalOnly:
		return slices.Contains(localProviders, provider)
	default:
		return false
	}
}

// lookupPolicy fetches and validates the policy for repoID.
func lookupPolicy(ctx context.Context, repos Repos, repoID string) (RepoPolicy, error) {
	if repoID == "" {
		return RepoPolicy{}, fmt.Errorf("request has no repository ID: %w", ErrUnknownRepo)
	}
	p, err := repos.Policy(ctx, repoID)
	if err != nil {
		return RepoPolicy{}, fmt.Errorf("looking up policy for repository %q: %w", repoID, err)
	}
	if err := p.validate(); err != nil {
		return RepoPolicy{}, fmt.Errorf("repository %q: %w", repoID, err)
	}
	return p, nil
}
