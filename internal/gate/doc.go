// Package gate implements Conclave's deterministic gates: build, lint,
// tests, diff size, secrets, protected paths, gate weakening, execution
// surface, and the flag-only tool_calls gate.
//
// Gates run against a frozen, hashed artifact, never against the agent's
// live workspace, so that what was checked is provably what gets pushed.
// The artifact is an envelope binding the diff to its base commit; its
// SHA-256 hex is both the blob key and the hash every GateResult carries.
//
// The order is fixed:
//
//  1. Freeze reads the sandbox's diff twice and requires identical bytes,
//     then stores the envelope. Nothing after it reads the workspace.
//  2. Gate loads the artifact through blob.Get, which re-hashes it.
//  3. The static gates run over the parsed diff, all of them: diff size,
//     protected paths, gate weakening, execution surface, tool_calls,
//     secrets. If any failed, the results are recorded and no sandbox is
//     touched.
//  4. SelectMode picks reset_in_place or clean_room.
//  5. Staging wipes /workspace (.git included) and /tmp, streams the
//     clean prepared tree back in from the worker, applies the recorded
//     diff with git configuration isolated, and requires a fresh diff to
//     equal it byte for byte.
//  6. Build, lint and tests run in that order and stop at the first
//     failure. The tests gate also requires a JUnit report showing at
//     least one test ran and none failed.
//  7. Every result is recorded in one transaction; the first writer wins.
//
// A gating environment failure returns ErrEnvironment and records
// nothing, so the attempt is errored and a retry starts clean. Verify is
// the single check before a push.
//
// A sandbox handed to Gate is never handed back to an agent. The sandbox
// image needs sh, env, find, mv, rm, chmod, head, cat, tar and git.
package gate
