# Ticket 1 — Repository skeleton and CI

**Status:** merged in `2574d49`, CI later refined in PR #2 and PR #5 · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-1--repository-skeleton-and-ci) · **Requirements:** NFR-M1, NFR-M8, NFR-PO1

## In one paragraph

The empty shape of the project: a Go module, one package per responsibility (each with a `doc.go` stating it), a Makefile, a Docker Compose stack for local development, a linter configured to reject complicated code, and a CI pipeline that runs every check on every pull request. No behaviour, just the frame everything else goes into.

## Why it exists

Every later ticket assumes the layout, the commands, and the checks are already there. Settling them first means no ticket argues about structure, and every ticket is held to the same bar from its first commit.

## How it works

- **Packages.** `ARCHITECTURE.md` lists the layout. Packages that later tickets fill (`agent`, `gate`, `github`, `workflow`, `activity`, `eval`, `repoindex`) exist now with only a `doc.go`, so their responsibilities are fixed before anyone writes code.
- **Makefile.** `make check` runs vet, lint, `go mod tidy -diff` plus `go mod verify`, race-enabled tests, Temporal's `workflowcheck`, and `govulncheck`. Tool versions are pinned in the Makefile and run with `go run`, so no tool needs a separate install.
- **Lint limits** (`.golangci.yml`): cyclomatic complexity 15, cognitive complexity 20, 60 lines or 50 statements per function, `nestif` 5, plus `gofmt` and `goimports`. A function that trips a limit gets restructured; the limit isn't raised.
- **CI** (`.github/workflows/ci.yml`) runs `make check-ci` on every PR. That is `check` with tests run uncached (`-count=1`) and with `CONCLAVE_REQUIRE_INTEGRATION=1`, which turns a skipped Docker test into a failure (see `internal/testenv`). `govulncheck` runs on `main` and weekly, because its result depends on a vulnerability database rather than on the diff. The `tidy` step, `check-ci`, and `internal/testenv` came later, in PR #5.
- **Dev stack** (`deploy/docker-compose.yml`): Postgres 16 with pgvector, the Temporal dev server (UI on 8233), and Jaeger (UI on 16686). Images are pinned to exact versions.
- **Claude Code guardrails** (`.claude/`, from the initial commit, before ticket 1): a hook that blocks tool calls touching secret material, the `implement-ticket` and `review-ticket` commands, and the `ticket-reviewer` agent.

## Where to look

| File | What's in it |
|---|---|
| `Makefile` | Every command, with the pinned tool versions |
| `.golangci.yml` | Lint and complexity limits |
| `.github/workflows/ci.yml` | What CI runs and when; comments explain the caching and the docs-only PR case |
| `deploy/docker-compose.yml` | Local Temporal, Postgres, Jaeger |
| `internal/testenv/` | Skip locally, fail in CI, when Docker or git's HTTP backend is missing (PR #5) |
| `.claude/` | Hooks, commands, and the reviewer agent |

## What the tests prove

At this stage the checks are the tests: `make check` passing on an empty skeleton showed the pipeline itself worked. Since PR #5, `internal/testenv` has its own test that a missing environment fails when CI requires it.

## Left open

- `cmd/server` and `cmd/worker` only log a line. Ticket 6 fills the server and ticket 8 the worker.
- The Temporal SDK is not in `go.mod` yet. `workflowcheck` runs, but has no workflows to check until ticket 8.

## Check your understanding

1. Why does CI run tests with `-count=1` when Go already caches test results?
2. What does `CONCLAVE_REQUIRE_INTEGRATION` change, and why would a security test that skips be dangerous?
3. Why does `govulncheck` not run on pull requests?
4. What should you do when a function exceeds `funlen`?
