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
// Requests are served by a Scoped router, bound to one repository, which
// carries a token budget and an idempotency key prefix: every call the
// provider bills is charged to the budget and recorded in the cost ledger,
// including calls that end in an error, and neither can be skipped. Errors
// a retry cannot fix are reported by IsPermanent.
//
// In production, DirectoryRepos takes each repository's tier, role
// allowlist, and sensitive providers from the owner's configuration, and
// refuses a repository whose database record disagrees on the tier.
package llm
