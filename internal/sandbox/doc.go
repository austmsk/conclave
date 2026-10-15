// Package sandbox holds what every sandbox backend shares: preparing a
// working tree on the worker before it enters a sandbox, and verifying that
// a tree has been prepared. The Sandbox and SandboxProvider interfaces
// themselves live in contracts; concrete providers live in subpackages:
// docker for the local Milestone 1 implementation, daytona for the remote
// provider added later.
//
// A sandbox holds no credentials, has no Docker socket, and gets no
// network access during the agent phase. PrepareTree is where the first of
// those is enforced (FR-SEC21): it strips remotes, credential helpers, and
// reflogs from a clone so nothing that entered the worker's trusted
// workspace during cloning can follow the tree into the sandbox.
package sandbox
