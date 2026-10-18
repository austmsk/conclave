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
// the sandbox user. Paths are checked lexically first (sandbox.CheckPath).
// The path then arrives as a script argument and is only ever expanded
// quoted with globbing off, so it is used exactly as given; the script
// refuses any symlink along the way, and a regular file with more than
// one link, which could be another name for .git/config. Those checks
// happen before the read or write, so a component swapped for a symlink
// in between would be followed. The contract asks for a root handle that
// closes that race; none exists inside a container.
//
// What narrows the race is that no process survives a sandbox command
// (see Process lifetime) and commands are serialized, so while a file
// script runs nothing else in the container is running to make the swap.
// That narrows the race; it does not close it. The residuals are a
// process in another PID namespace, which the init's kill cannot reach
// and which the sandbox as configured cannot create (no capabilities, no
// new privileges), and, as before, damage that stays inside the
// container: a misdirected write lands somewhere the agent could reach
// with a shell anyway, and the diff omits it. This is a different
// mechanism from FR-SEC29's fallback, which resolves the parent through
// symlinks and compares relative paths, and so a deviation from it; the
// decision record is owed.
//
// # Process lifetime
//
// PID 1 is a shell loop that traps SIGUSR1 and answers it with kill -9 -1,
// which kills every other process in the container (the caller is exempt),
// and then starts a fresh sleep. After every sandbox command (Exec,
// ReadFile, WriteFile, Diff and the tree upload alike) and on every path
// out of it, errors included, the provider sends that signal through the
// Engine API while it still holds the command lock. It then confirms
// through the daemon's process list that only the init and a sleep newer
// than the signal remain. The signal is always sent: a count taken
// beforehand can be fooled by a stray that kills the init's sleep. Diff
// needs this as much as Exec does, because git add honours
// core.fsmonitor, which the agent can set. Signalling rather than running
// kill in the container means no new process has to be spawned inside a
// possibly saturated pids cgroup. If the init does not respond, the
// container is killed outright and the sandbox reports ErrKilled from
// then on.
//
// Install steps go through Exec as well, so a daemon an install step
// starts survives neither into the agent phase nor into the next step:
// each install step must be self-contained.
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
