// Package llm implements the role-based model router and its provider
// adapters, plus a fake replay provider for tests.
//
// Every model request passes through the router, which enforces a
// repository's sensitivity tier before the request reaches a provider. A
// repository with no tier, or a fallback whose provider the tier
// disallows, is an error and never a silent downgrade to something
// permissive. Which providers each tier allows is a table in code, not
// configuration, so configuration cannot widen a tier.
//
// Requests are served by a Scoped router, which carries a token budget and
// an idempotency key prefix: every call is charged to the budget and
// recorded as a cost event, and neither can be skipped.
package llm
