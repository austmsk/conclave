# Milestone 1 — Walking Skeleton

**Done when:** a labeled issue on the Election Tally repository produces a pull request that passes that repository's CI in at least 7 of 10 trials, with zero orphaned sandboxes across 10 runs including at least 2 cancellations, and every run traceable end to end in the Temporal UI and the database.

**Scope:** one issue, one implementer, one sandbox, one pull request. No planner, no judge, no test writer, no merge queue. The implementer writes its own tests; the test writer and hidden-test split arrive at the start of Milestone 2 (`FR-S7`).

## Working the list

Each ticket is one Claude Code session. Before starting, read the review-list items it names. After CI passes, run `/review-ticket <n>`, fix what the reviewer finds, then merge and write the decision record.

Model column: **F** = Fable 5.1 (tickets with review-list items or subtle correctness), **S** = Sonnet 5 (mechanical). Refine each ticket with Opus in plan mode first.

| # | Ticket | Model | Review list |
|---|---|---|---|
| 1 | Repository skeleton and CI | S | — |
| 2 | Contracts package and schema | F | RL-4, RL-13 |
| 3 | Docker sandbox | F | RL-5, RL-3 |
| 4 | LLM router and fake provider | F | RL-17, RL-6 |
| 5 | Agent loop and tools | F | RL-6, RL-11, RL-29* |
| 6 | GitHub App and webhooks | F | RL-7, RL-8 |
| 7 | Gates and artifact integrity | F | RL-2 |
| 8 | Temporal workflow | F | RL-1, RL-2, RL-3, RL-9, RL-14, RL-16 |
| 9 | Pull request delivery | F | RL-2, RL-21 |
| 10 | End-to-end run and hardening | F | RL-18 |

\* Path validation does not yet have its own review-list ID; see `FR-SEC29`.

---

## Ticket 1 — Repository skeleton and CI

Go module `github.com/austmsk/conclave`, the package layout from `ARCHITECTURE.md` with a `doc.go` in each package, a Makefile with the targets named in `CLAUDE.md`, a Docker Compose file bringing up Temporal dev server, Postgres with pgvector, and Jaeger, and a GitHub Actions workflow.

CI runs `go vet`, `golangci-lint` (with `gocyclo`, `gocognit`, `funlen`, `nestif` enabled), `go test -race`, Temporal's `workflowcheck`, and `govulncheck`.

**Done when:** `make check` passes on an empty skeleton, `make dev` brings the stack up, and CI is green on the first commit.

**Requirements:** NFR-M1, NFR-M8, NFR-PO1

---

## Ticket 2 — Contracts package and schema

Port `internal/contracts/` from the spec pack. Write the Postgres migrations for repos, feature requests, plans, tasks, attempts, gate results, decisions, cost events, and state transitions. Write the transition table and the compare-and-set update helper.

Cost events and state transitions are append-only: grant the application role insert-only access. Add the partial unique index for one approved plan per feature request.

**Done when:** migrations apply and roll back cleanly; the transition table has an exhaustive test covering every legal and illegal transition; an attempted update from a wrong state is distinguishable from an already-applied one; a test proves the application role cannot update or delete an append-only row.

**Requirements:** FR-SEC10, NFR-M7 · **Review list:** RL-4, RL-13

**Review this one closely yourself.** Everything else is built behind these types.

---

## Ticket 3 — Docker sandbox

Implement `SandboxProvider` and `Sandbox` over the Docker Go SDK. Non-root user, all capabilities dropped, `no-new-privileges`, process and disk limits, no Docker socket, read-only root filesystem where the toolchain allows. Two-phase network: registries during install, none during the agent phase. Environment allowlist only. Working tree prepared on the worker with remotes and credentials stripped.

Implement `Reap`.

**Done when:** integration tests cover create, exec with timeout, file read and write, diff, and close; attack tests confirm that an outbound network call fails during the agent phase, host paths are unreadable, the process is non-root, the Docker socket is absent, a fork bomb is contained, and the disk quota holds; `Close` is safe to call twice; `Reap` removes a deliberately orphaned container.

**Requirements:** FR-SEC4, FR-SEC21, FR-SEC23, NFR-R3 · **Review list:** RL-5, RL-3

---

## Ticket 4 — LLM router and fake provider

Implement `Router` with the Anthropic adapter and a fake replay provider that serves recorded responses from fixtures. Role-to-model configuration in YAML, validated at startup. Tier enforcement: the repository's tier is checked on every request including fallbacks; a repository with no tier is an error.

Record cost per request. Enforce per-attempt token budgets.

**Done when:** the policy matrix test covers every tier and role combination; a request for a repository with no tier fails; a fallback whose provider the tier disallows fails rather than proceeding; the fake provider replays multi-turn conversations with tool calls; no test reaches a real provider.

**Requirements:** FR-I2, FR-SEC12, FR-SEC13, FR-SEC9 · **Review list:** RL-17, RL-6

---

## Ticket 5 — Agent loop and tools

Implement `AgentRunner` and the Milestone 1 tools: `read_file`, `list_dir`, `search`, `write_file`, `apply_patch`, `run_tests`, `run_build`, `run_lint`, `run_command`, `finish`. Configured command tools take their command strings from repository configuration, not from the model.

Path validation through a workspace root handle (`os.Root`). Reject absolute paths, `..` segments, encoded traversal, and writes to `.git` internals. Reject, never sanitize.

Enforce every limit in `Limits`. Validate the finish report against its schema with exactly one repair attempt. Record every tool call from the worker with full arguments, exit code, duration, and a blob-store pointer for full output.

**Done when:** the loop completes a task against the fake provider; each limit is separately tested; path escape tests cover symlinks, a component swapped for a symlink between validation and write, a sibling directory sharing a prefix with the workspace root, and `....//`; an invalid finish report triggers one repair then fails with `invalid_output`; every tool call appears in the audit table.

**Requirements:** FR-SEC3, FR-SEC24, FR-SEC29 · **Review list:** RL-6, RL-11

---

## Ticket 6 — GitHub App and webhooks

Webhook receiver on `net/http` with constant-time signature verification. Command parser honouring only the owner's numeric user ID; comments from the App itself and from other bots are ignored. App authentication via `ghinstallation`. Clone, branch push restricted to `agent/` prefixes, PR creation, comment posting and in-place editing, reaction adding.

Webhooks acknowledge within two seconds and dispatch work asynchronously.

**Done when:** invalid signatures are rejected; a command from a non-owner ID is ignored and logged; a command in an App-authored comment triggers nothing; a duplicate webhook delivery does not start a second workflow; an installation token is obtained and used; no token appears in any log or trace.

**Requirements:** FR-I1, FR-SEC1, FR-SEC2, FR-SEC5, FR-SEC15, FR-SEC28, NFR-P1 · **Review list:** RL-7, RL-8

**Manual setup:** register the App (name it at registration; `Conclave` if free, otherwise `Conclave Agent`), grant contents, pull requests, issues, checks, and metadata — never `workflows` — install on the Election Tally repository only, and enable branch protection on its default branch with no App bypass.

---

## Ticket 7 — Gates and artifact integrity

Implement the Milestone 1 gates: build, lint, tests, diff size, secrets (`gitleaks`), protected paths, gate weakening, and execution surface. Implement both gating modes; default `reset_in_place`.

Freeze first: kill sandbox processes, extract the diff, hash it, then gate that recorded artifact. Record the hash on every gate result.

**Done when:** each gate has a fixture that fails it; an attempt that deletes or skips a test is rejected; a diff touching `package.json` scripts, `.husky/`, `.gitattributes`, or `.claude/` is flagged; a gate result's artifact hash matches the diff that was checked; an environment build failure yields `errored`, not `failed_gates`.

**Requirements:** FR-A1, FR-A3, FR-SEC25, FR-SEC27 · **Review list:** RL-2

---

## Ticket 8 — Temporal workflow

The feature workflow: analyze, implement, gate, deliver. Activities for every side effect. Signal handlers for `/approve`, `/cancel`, `/retry`. Workflow ID derived from the issue number. Deferred cleanup using `workflow.NewDisconnectedContext`. Status comment updated in place.

Milestone 1 statuses only: feature requests use the six-status model; tasks use `running`, `done`, `needs_attention`, `cancelled`; attempts use `running`, `passed`, `failed`, `errored`.

**Done when:** `workflowcheck` passes; the workflow is tested with `testsuite` without a server; a simulated worker crash mid-attempt resumes on another worker; a cancellation during the agent phase still releases the sandbox; every activity has an idempotency test that runs it twice and asserts one effect; no map is ranged over without sorting.

**Requirements:** NFR-R1, NFR-R2, NFR-R3 · **Review list:** RL-1, RL-2, RL-3, RL-9, RL-14, RL-16

---

## Ticket 9 — Pull request delivery

Minimal `pr_writer` role. Branch model: integration branch pushed after the task lands, `pr-1` cut from it. Commits authored by the App's bot identity, one squashed commit per task, Conventional Commits with `Task`, `Plan`, `Model`, and `Generated-by` trailers. Rebase onto the default branch, rerun gates, then open the PR as a draft; flip to ready when the repository's CI passes.

Record each PR's base SHA for later restacking. Push refuses any artifact whose hash does not match a passing gate result.

**Done when:** a PR opens with the described body including the risk section; the push path rejects a tampered artifact; a base SHA is recorded; branch cleanup runs after merge; commits show the bot as author.

**Requirements:** FR-S10, FR-S16, FR-S17, FR-S19, FR-A5, FR-A7, FR-SEC27 · **Review list:** RL-2, RL-21

---

## Ticket 10 — End-to-end run and hardening

Run the full path against the Election Tally repository ten times, including two cancellations. Fix what breaks. Add the orphan reaper as a scheduled workflow. Add the kill switch. Add the local append-only alert log and the alert-issue path.

**Done when:** the 7-of-10 success criterion is met; no orphaned containers remain; the kill switch cancels everything in flight; a deliberately triggered alert opens an issue and is written to the local log; a `docs/decisions/` record exists for every non-obvious choice made during the milestone.

**Requirements:** FR-SEC11, FR-SEC31, NFR-R3, NFR-M9 · **Review list:** RL-18

---

## Not in this milestone

Planner, judge, test writer, codebase brief, merge queue, stacked PRs, multiple providers, embeddings, Daytona, multi-machine workers, evaluation harness, canary tokens, prompt injection fixtures, dependency-addition tool, and self-runs on the Conclave repository (`FR-SEC26` forbids these before Milestone 2).
