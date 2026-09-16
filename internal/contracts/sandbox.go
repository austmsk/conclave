package contracts

import (
	"context"
	"time"
)

// NetworkMode controls a sandbox's network access. The install phase runs
// under NetworkRegistries with platform code in control; the network is cut to
// NetworkNone before the agent is given control, so a manipulated agent has no
// path out (FR-SEC4).
type NetworkMode string

const (
	NetworkNone       NetworkMode = "none"
	NetworkRegistries NetworkMode = "registries"
)

// ResourceLimits bound a sandbox. Every field is required; a zero value is a
// configuration error rather than "unlimited".
type ResourceLimits struct {
	MemoryBytes int64
	CPUShares   int64
	DiskBytes   int64
	MaxProcs    int64
	Timeout     time.Duration
}

// SandboxSpec describes a sandbox to create.
type SandboxSpec struct {
	// Image is pinned by digest, never by tag (FR-SEC30).
	Image string

	// TreePath is the prepared working tree on the worker: cloned at the base
	// commit, excluded files substituted, git remotes and credentials
	// stripped (FR-SEC21, FR-SEC22).
	TreePath string

	// Env is an allowlist. The worker's own environment is never inherited,
	// and values for external services are canaries (FR-SEC23).
	Env map[string]string

	Network NetworkMode
	Limits  ResourceLimits
}

// ExecRequest is one command to run inside a sandbox.
type ExecRequest struct {
	Command []string
	WorkDir string
	Timeout time.Duration
}

// ExecResult is the outcome of an ExecRequest. Output is captured in full by
// the worker for the audit trail, and truncated separately for the model.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
	Duration time.Duration
}

// Sandbox is an isolated environment in which untrusted code runs. Docker and
// Daytona implementations satisfy the same interface, so nothing above this
// layer knows which backend served a given attempt.
//
// Close must be safe to call more than once and must be invoked from a
// disconnected context in deferred cleanup, or a cancelled workflow leaves the
// container running (RL-3).
type Sandbox interface {
	ID() string

	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// ReadFile and WriteFile operate through a workspace root handle so
	// containment holds against symlinks and the check-then-write race
	// (FR-SEC29). Paths are relative to the workspace root.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error

	// Diff returns the unified diff of the workspace against its base commit.
	// Substitution commits are excluded by construction, since the diff is
	// taken from the substitution commit forward.
	Diff(ctx context.Context) ([]byte, error)

	Close(ctx context.Context) error
}

// SandboxProvider creates sandboxes.
type SandboxProvider interface {
	Create(ctx context.Context, spec SandboxSpec) (Sandbox, error)

	// Reap destroys sandboxes older than the given age that the provider still
	// knows about. Backs the orphan reaper (NFR-R3).
	Reap(ctx context.Context, olderThan time.Duration) (int, error)
}
