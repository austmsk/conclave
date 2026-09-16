// Package sandbox defines the Sandbox and SandboxProvider interfaces that
// isolate agent execution. Concrete providers live in subpackages: docker
// for the local Milestone 1 implementation, daytona for the remote
// provider added later.
//
// A sandbox holds no credentials, has no Docker socket, and gets no
// network access during the agent phase.
package sandbox
