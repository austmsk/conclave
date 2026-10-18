// Package llm implements the role-based model router and its provider
// adapters, plus a fake replay provider for tests.
//
// Every model request passes through the router, which enforces a
// repository's sensitivity tier before the request reaches a provider. A
// repository with no tier, or a fallback whose provider the tier
// disallows, is an error and never a silent downgrade to something
// permissive. Which providers the public, private, and local-only tiers
// allow is a table in code, not configuration, so configuration cannot
// widen them. The sensitive tier is the one case where the list is
// configuration: an owner-approved list per repository, which may name only
// known providers.
//
// Requests are served by a Scoped router, bound to one repository and
// attributed to a feature request and, for attempt work, an attempt. Every
// call reserves its claim in the cost ledger against the three FR-SEC9
// caps (the attempt's token budget, the feature request's token cap, and
// the monthly USD cap) before a provider is reached, and every call the
// provider bills is recorded, including calls that end in an error. A cap
// refusal is a BudgetError naming the cap. Errors a retry cannot fix are
// reported by IsPermanent.
//
// In production, DirectoryRepos takes each repository's tier, role
// allowlist, and sensitive providers from the owner's configuration, read
// by LoadRepoAccess from deploy/repos.yaml, and refuses a repository whose
// database record disagrees on the tier.
package llm
