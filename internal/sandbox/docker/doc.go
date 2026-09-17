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
//     so the disk quota is enforced by the kernel rather than by policy;
//   - memory and swap capped at MemoryBytes, a pids cgroup limit of
//     MaxProcs, and CPU shares of CPUShares;
//   - no bind mounts of any kind, so no host path and no Docker socket is
//     reachable; the working tree is streamed in as a tar archive, and
//     files are read back through exec as the sandbox user;
//   - a private IPC namespace and a private PID namespace.
//
// # Network phases
//
// A sandbox created with NetworkRegistries is attached to a bridge network
// of its own for the install phase, during which platform code runs the
// repository's install command. DisconnectNetwork detaches it, after which
// the container has only a loopback interface; that call must precede
// handing control to the agent. A sandbox created with NetworkNone never
// has a network. The registry allowlist inside the install phase is not
// yet enforced by this provider: the install phase sees the network the
// worker sees, which is acceptable while the only inputs to that phase are
// the owner's own lockfiles.
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
// Close removes the container and its network and is safe to call more
// than once. Every container and network carries the labels
// conclave.sandbox and conclave.sandbox.namespace; Reap lists by those
// labels and removes anything older than the given age, so a worker that
// died mid-attempt leaves nothing behind once the reaper runs. Reap only
// touches its own namespace, so a test run cannot destroy a developer's
// sandboxes on the same daemon.
package docker
