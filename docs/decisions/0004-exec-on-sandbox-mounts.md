# 0004 — Sandbox mounts allow exec

**Date:** 2026-09-16
**Status:** accepted

## Context

The Docker sandbox's root filesystem is read-only. The only writable space is three tmpfs mounts: the workspace, the agent's home, and `/tmp`. Docker mounts every tmpfs with `noexec` unless told otherwise, and the first implementation did not tell it otherwise. The attack tests passed, because every test command ran `/bin/sh` from the read-only image. Nothing built inside the sandbox could run: `go test` writes its test binary to `$TMPDIR` and executes it, `node_modules/.bin` lives in the workspace, and a repository's own scripts live there too. Milestone 1's exit criterion is a pull request that passes the target repository's CI, which is unreachable from a sandbox that cannot execute what it builds.

## Decision

The three writable mounts are mounted with `exec`, and keep `nosuid` and `nodev`.

`noexec` is a real control, not a flag: it stops code that arrives at runtime from running. That is exactly the property a build-and-test sandbox cannot have, because building code and running it is the sandbox's purpose. The boundary against that code is the container: non-root user, every capability dropped, `no-new-privileges`, no network in the agent phase, no host mounts, and cgroup limits on memory, pids and disk. `nosuid` stays because a setuid binary planted in the workspace would otherwise run as its owner, and `nodev` stays because nothing legitimate creates device nodes in a working tree; neither costs a toolchain anything.

## Consequences

Any code the agent writes or installs can execute inside the sandbox, which is the intended state and is what every gate assumes. The cost is that the tmpfs mounts no longer add a second layer against runtime-arriving code; the container is the only layer, and its settings are now load-bearing in a way that a test must guard. `TestExecRunsBinariesFromWritableMounts` copies a real binary into each mount and runs it, and runs a script from the workspace, so a regression to `noexec` fails loudly instead of surfacing in the first CI run against a real repository. The `setuid` assertion in the same test guards `nosuid`.
