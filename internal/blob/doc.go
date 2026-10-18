// Package blob stores the full output of tool calls, addressed by content,
// so the audit trail can point at what a truncated result left out
// (FR-SEC24). Keys are <namespace>/<sha256 hex>, where the namespace is
// the repository ID: storing the same output twice yields the same key, and
// retention can later differ per repository's sensitivity tier.
package blob
