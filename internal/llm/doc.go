// Package llm implements the role-based model router and its provider
// adapters, plus a fake replay provider for tests.
//
// Every model request passes through the router, which enforces a
// repository's sensitivity tier before the request reaches a provider. A
// repository with no tier, or a fallback whose provider the tier
// disallows, is an error and never a silent downgrade to something
// permissive.
package llm
