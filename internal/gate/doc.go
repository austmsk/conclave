// Package gate implements Conclave's deterministic gates: build, lint,
// tests, diff size, secrets, protected paths, gate weakening, and
// execution surface.
//
// Gates run against a frozen, hashed artifact extracted after the sandbox
// is killed, never against a live sandbox, so that what was checked is
// provably what gets pushed.
package gate
