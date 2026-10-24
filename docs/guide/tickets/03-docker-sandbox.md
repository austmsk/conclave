# Ticket 3 — Docker sandbox

**Status:** merged in `e894259` (feature `82de9d8`, review fixes `729e0b5`) · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-3--docker-sandbox) · **Requirements:** FR-SEC4, FR-SEC21, FR-SEC23, NFR-R3 · **Review list:** RL-5, RL-3

> **Changed after merge — `writeTree` moved (ticket 7).**
> - **When:** 2026-10-23, in ticket 7's groundwork commit on branch `ticket/7` (COMMIT_PENDING).
> - **Where:** `writeTree` and its helper `writeEntry` moved from `internal/sandbox/docker/archive.go` to `internal/sandbox/archive.go`. The function is now exported as `sandbox.WriteTree`. `copyTreeIn` stays in `docker/archive.go` and calls it.
> - **How:** the code moved without changes: same signature apart from the capital letter, same symlink, uid/gid and special-file handling. The docker tests that cover streaming the tree in pass unchanged.
> - **Why:** ticket 7's gate staging (`internal/gate`) wipes a sandbox's workspace and streams the clean prepared tree back in, through the `contracts.Sandbox` interface rather than the docker backend. One shared tar writer means the gate and any later backend (Daytona) apply the same rules for what a tree may contain. The alternative was a second copy that could drift. The owner chose the move at ticket 7's start.

## In one paragraph

A place to run untrusted code. `internal/sandbox` clones a repository on the worker and strips everything that could leak a credential. `internal/sandbox/docker` copies that tree into a locked-down container and runs commands, reads and writes files, and takes a diff, all inside it. Whatever ends the attempt, the container is removed. The design assumes the agent inside is hostile, and makes the worst case a wasted container.

## Why it exists

The agent runs code it wrote, or code it was tricked into writing. The sandbox makes sure that code can't reach a credential, the network (after install), the host's files, the Docker daemon, or the worker's environment. It also can't exhaust the machine: memory, processes and disk are all capped (`RL-5`). Sandboxes must never be left running, whatever happens to the workflow (`RL-3`, `NFR-R3`).

## How it works

**1. Clone on the worker** (`sandbox.Clone`, `clone.go`). Git runs with a scrubbed environment: no global or system config, no prompts. The installation token travels only as an `http.extraheader` set through `GIT_CONFIG_COUNT/KEY/VALUE` environment variables, so it is never written to `.git/config`, the reflog, or disk. A wrong token leaves no tree behind.

**2. Prepare the tree** (`sandbox.PrepareTree`, `tree.go`). This rewrites `.git/config` from an allowlist of what to keep, rather than deleting a list of known-bad keys. It also drops remotes, credential helpers, reflogs, and remote refs in `packed-refs`, and covers submodules under `.git/modules/**`. `VerifyTree` checks a tree is clean, and the provider refuses an unprepared one, as defence in depth.

**3. Create the container** (`Provider.Create`, `spec.go`, `provider.go`). Every container gets the same hardening, and nothing in `SandboxSpec` can relax it:

- uid 1000, every capability dropped, `no-new-privileges`
- read-only root filesystem. The only writable space is three tmpfs mounts (workspace ½, home ⅜, tmp ⅛ of `DiskBytes`), so the kernel enforces the disk quota. Docker can't quota a container's disk on overlayfs. tmpfs counts against memory, so `DiskBytes` must be below `MemoryBytes`.
- the mounts allow exec, because a build sandbox has to run what it builds. The container is the boundary, not `noexec` ([decision 0004](../../decisions/0004-exec-on-sandbox-mounts.md)).
- memory, swap, pids and CPU caps; private IPC and PID namespaces
- no bind mounts at all, so no host path and no Docker socket. Image-declared `VOLUME`s are covered with a 4 KiB read-only tmpfs, and `Create` refuses any container that still ends up with a mount.
- environment: a fixed set plus the spec's allowlist. The worker's environment is never inherited.
- the tree is streamed in with `tar -x` run inside the container. The Engine archive API refuses read-only containers, so this is a copy, never a mount.

**4. Two network phases.** `Create` always returns a container with no network, and refuses a spec asking for `NetworkRegistries`. `CreateWithInstall` attaches a private bridge, runs the install commands, disconnects, and only then returns the sandbox. If install fails, the sandbox is closed and never handed out. `contracts.Sandbox` has no way to cut a network, so a networked sandbox handed to a caller would stay networked.

**5. Run commands** (`exec.go`). PID 1 is a small shell loop that answers SIGUSR1 with `kill -9 -1`. When a command times out or its output passes the cap (32 MiB default), the provider sends that signal through the daemon. It doesn't need to start a new process, which matters because a fork bomb may have used up the pid limit. If PID 1 doesn't respond, the whole container is killed.

**6. Files and diff.** `ReadFile` and `WriteFile` run a shell script inside the container as the sandbox user. The path is passed as `$1`, only ever expanded inside quotes, with globbing off, and any symlink along the path is refused. `Diff` runs `git` inside the container, so a tampered `.git` can't execute code on the worker.

**7. Clean up** (`Close`, `Reap`). `Close` force-removes the container and its network, is safe to call twice, and doesn't wait for a running command. Everything is labeled `conclave.sandbox` and `conclave.sandbox.namespace`. `Reap(olderThan)` sweeps its own namespace at the given age, and every namespace past `StaleAge` (24 h default), carrying on past failures.

## Where to look

| File | What's in it |
|---|---|
| `internal/sandbox/clone.go` | Tokenless clone |
| `internal/sandbox/tree.go` | `PrepareTree`, `VerifyTree`, `HeadCommit` |
| `internal/sandbox/archive.go` | `WriteTree`, the tar writer (moved here from `docker/archive.go` in ticket 7) |
| `internal/sandbox/docker/doc.go` | The full isolation story, worth reading in full |
| `internal/sandbox/docker/spec.go` | Spec validation, host config, disk split, environment |
| `internal/sandbox/docker/provider.go` | `Create`, `CreateWithInstall`, `Reap` |
| `internal/sandbox/docker/sandbox.go`, `exec.go` | Exec, file operations, diff, close |
| `internal/sandbox/sandboxtest/` | Test repositories that carry a canary token |
| `internal/contracts/sandbox.go` | The `Sandbox` and `SandboxProvider` interfaces |

## What the tests prove

The attack tests (`TestAttack*` in `docker/`) try each escape and assert it fails:

| Attack | Test |
|---|---|
| Outbound network with no network, after install, or via `Create` with registries | `TestAttackOutboundNetworkFailsWithNetworkNone`, `…AfterInstallPhase`, `TestAttackRegistriesIsRefusedThroughCreate` |
| A failed install leaks a networked sandbox | `TestAttackFailedInstallNeverHandsOutANetworkedSandbox` |
| Reading host paths, finding the Docker socket | `TestAttackHostPathsUnreadable`, `TestAttackDockerSocketAbsent` |
| Running as root or with capabilities | `TestAttackProcessIsNonRootWithoutCapabilities` |
| Fork bomb, filling the disk | `TestAttackForkBombIsContained`, `TestAttackDiskQuotaHolds` |
| Inheriting the worker's environment | `TestAttackWorkerEnvironmentNotInherited` |
| Symlinks, path expansion, the symlink-swap race | `TestAttackFileOperationsRefuseSymlinks`, `TestAttackFilePathsAreNeverExpanded`, `TestAttackSymlinkSwapRaceStaysInsideTheContainer` |
| Token reaching the sandbox; unprepared tree accepted | `TestAttackNoTokenReachesTheSandbox`, `TestAttackUnpreparedTreeIsRefused` |

Plus lifecycle tests: timeouts kill the process tree, cancellation kills the command, `Close` doesn't block, a failed `Create` leaves nothing behind, and `Reap` removes orphans and respects namespaces. In CI these must run; a missing Docker daemon fails the build instead of skipping.

## Left open

- **The install phase has unrestricted egress**, not the registry allowlist `FR-SEC4` asks for. A malicious install script can reach the internet, and on a Linux worker, services the host binds on all interfaces. Closing this needs an egress proxy. It is a tracked follow-up.
- **`CreateWithInstall` isn't on the `contracts.SandboxProvider` interface.** Ticket 8's activity must either hold the concrete `*docker.Provider` or get a contract change (for example `Install []ExecRequest` on `SandboxSpec`). That needs a hand review.
- **The symlink check happens before the read or write**, so a path swapped in between is followed. The effect stays inside the same container, which the agent could reach with a shell anyway, and is documented in `doc.go`.
- Ticket 3's choices (tmpfs quota, tar copy, in-container git, PID 1 signal, volume masking) have no decision records yet beyond 0004.

## Check your understanding

1. Why is the disk quota three tmpfs mounts, and why must `DiskBytes` be below `MemoryBytes`?
2. Why does `Create` refuse `NetworkRegistries` instead of just attaching a network?
3. A fork bomb has filled the pid limit and a command has timed out. How does the provider stop it without starting a new process?
4. Why is the tree copied in with `tar` inside the container rather than bind-mounted or sent with the archive API?
5. Where can the installation token exist during a clone, and where can it never be?
6. What does `Reap` do differently for its own namespace and for others?
