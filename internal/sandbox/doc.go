// Package sandbox holds what every sandbox backend shares: preparing a
// working tree on the worker before it enters a sandbox, verifying that a
// tree has been prepared, archiving it for streaming in (WriteTree), the
// workspace path rules (CheckPath), and the
// refusals every backend reports alike (ErrInvalidPath and the rest), so
// the agent loop can tell a refusal the model should see from a failure of
// the sandbox itself. The Sandbox and SandboxProvider interfaces live in
// contracts; concrete providers live in subpackages: docker for the local
// Milestone 1 implementation, daytona for the remote provider added later.
//
// It also holds the rules every backend applies to a SandboxSpec
// (DigestPinned, ValidEnvKey, ReservedEnv, CheckLimits) and the
// per-repository sandbox configuration that feeds one (LoadConfig, which
// reads deploy/sandboxes.yaml). LoadConfig applies the same rules, so a
// configuration a provider's Create would refuse stops the worker at
// startup instead of failing every attempt.
//
// A sandbox holds no credentials, has no Docker socket, and gets no
// network access during the agent phase. PrepareTree is where the first of
// those is enforced (FR-SEC21): it strips remotes, credential helpers, and
// reflogs from a clone so nothing that entered the worker's trusted
// workspace during cloning can follow the tree into the sandbox.
//
// CommitDiff builds the platform's commits on the worker, for the push
// path: a diff applied to a temporary index read from the parent, never
// checked out, with the identity and date fixed by the caller, so the
// same inputs give the same commit ID in any clone.
package sandbox
