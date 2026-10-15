// Package docker implements contracts.SandboxProvider and contracts.Sandbox
// over the Docker Engine API for the local Milestone 1 worker.
//
// # Isolation (RL-5, FR-SEC4)
//
// Every container runs with the same hardened settings, none of which the
// caller can relax through SandboxSpec:
//
//   - a non-root user (uid 1000 by default), every capability dropped, and
//     no-new-privileges, so a compromised process cannot regain what it
//     lacks through setuid binaries;
//   - a read-only root filesystem; the only writable space is three tmpfs
//     mounts (workspace, home, tmp) whose sizes together equal DiskBytes,
//     so the disk quota is enforced by the kernel rather than by policy.
//     Those mounts allow exec, because a build-and-test sandbox has to run
//     what it builds; the container, not noexec, is the boundary
//     (docs/decisions/0004). tmpfs is memory-backed, so DiskBytes must be
//     below MemoryBytes and the spec is refused otherwise;
//   - memory and swap capped at MemoryBytes, a pids cgroup limit of
//     MaxProcs, and CPU shares of CPUShares;
//   - no bind mounts of any kind, so no host path and no Docker socket is
//     reachable; image-declared volumes are covered with a read-only tmpfs
//     and Create refuses any container that ends up with a mount; the
//     working tree is streamed in as a tar archive, and files are read
//     back through exec as the sandbox user;
//   - a private IPC namespace and a private PID namespace.
//
// # Network phases
//
// Create only ever returns a sandbox without a network. A spec asking for
// NetworkRegistries must go through CreateWithInstall, which attaches a
// bridge network of the sandbox's own, runs the install commands under
// platform control, disconnects the network, and only then returns. That
// is deliberate: nothing on contracts.Sandbox can cut a network, so a
// networked sandbox handed to a caller would stay networked and nothing
// would fail. After disconnection the container has only loopback.
//
// The install phase itself is unrestricted egress, not the registry
// allowlist FR-SEC4 asks for. A malicious lifecycle script in a lockfile
// dependency has full network access during that phase and, on a Linux
// worker, can reach services the host binds on all interfaces through the
// bridge gateway. Closing that needs an egress proxy on the worker and an
// internal network; it is a tracked follow-up, not something this package
// mitigates.
//
// # File operations
//
// ReadFile and WriteFile run a small shell script inside the container as
// the sandbox user. The path arrives as a script argument and is only
// ever expanded quoted with globbing off, so it is used exactly as given;
// the script refuses any symlink along the way. That check happens before
// the read or write, so a component swapped for a symlink in between is
// followed. The contract asks for a root handle that closes that race;
// none exists inside a container. The consequence is bounded: the write
// lands somewhere else inside the same container, which the agent could
// reach with a shell anyway, and the diff simply omits it. Path validation
// keeps the diff honest; the container is the boundary (FR-SEC29).
//
// # Process lifetime
//
// PID 1 is a shell loop that traps SIGUSR1 and answers it with kill -9 -1,
// which kills every other process in the container (the caller is exempt).
// The provider sends that signal through the Engine API when an exec
// times out or overruns its output limit, so runaway processes are stopped
// without a new process having to be spawned inside a possibly saturated
// pids cgroup. If the init does not respond, the container is killed
// outright and the sandbox reports itself dead.
//
// # Cleanup (RL-3, NFR-R3)
//
// Close removes the container and its network, is safe to call more than
// once, and never waits for a running command: force removal ends the
// command, whose Exec then returns ErrClosed. Every container and network
// carries the labels conclave.sandbox and conclave.sandbox.namespace.
// Reap sweeps the provider's namespace at the requested age and every
// namespace at Config.StaleAge, continuing past any removal that fails,
// so a worker that died mid-attempt, a renamed namespace, or one wedged
// container leaves nothing behind for long. Reap's first sweep is scoped
// to its own namespace, so a test run cannot destroy a developer's fresh
// sandboxes on the same daemon.
package docker
