# Milestone 1 — Walking Skeleton

**Done when:** a labeled issue on the Election Tally repository produces a pull request that passes that repository's CI in at least 7 of 10 trials, with zero orphaned sandboxes across 10 runs including at least 2 cancellations, and every run traceable end to end in the Temporal UI and the database.

**Scope:** one issue, one implementer, one sandbox, one pull request. No planner, no judge, no test writer, no merge queue. The implementer writes its own tests; the test writer and hidden-test split arrive at the start of Milestone 2 (`FR-S7`).

## Working the list

Each ticket is one Claude Code session. Before starting, read the review-list items it names and `m1-exit-checklist.md`, which maps every requirement to its ticket and proving test and lists what Milestone 2 must find in place. After CI passes, run `/review-ticket <n>`, fix what the reviewer finds, then merge and write the decision record. The PR description names the test for each requirement ID and updates the checklist.

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
| 8a | Budgets and attribution | F | RL-2 |
| 8-pre | Workflow prerequisites | F | RL-2, RL-3, RL-13, RL-14 |
| 8 | Feature workflow core | F | RL-1, RL-2, RL-3, RL-13, RL-14, RL-16 |
| 8b | Commands and status surface | F | RL-9, RL-13 |
| 9 | Pull request delivery | F | RL-2, RL-21 |
| 10a | Kill switch, alerts and scheduled reaper | F | RL-18 |
| 10b | End-to-end trials | F | RL-18 |

\* Path validation does not yet have its own review-list ID; see `FR-SEC29`.

**Working order:** 1–4, then **8a**, 5, 6, 7, **8-pre**, 8, **8b**, 9, **10a**, **10b**. Ticket 8 is split three ways (see `Ticket 8 Split — Conclave.md` for the rationale), and 8-pre was added on 2026-10-26 after a review of the merged code found the workflow had nothing to call for its first steps (see `ticket-8-pre-plan.md`, whose second half also binds ticket 8). 8a goes before Ticket 5 because the runner has to tell an attempt budget hit from a platform cap hit, and 8a takes migration 00005.

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

Webhook receiver on `net/http` with constant-time signature verification. Command parser honouring only the owner's numeric user ID; comments from the App itself and from other bots are ignored. App authentication on `net/http`: the App JWT and installation tokens are written in the package, not taken from `ghinstallation` ([0010](../decisions/0010-github-client-on-net-http.md)). Clone, branch push restricted to `agent/` prefixes, PR creation, comment posting and in-place editing, reaction adding.

Webhooks acknowledge within two seconds and dispatch work asynchronously.

Dispatch goes through a small workflow-starter interface. The Temporal SDK arrives in Ticket 8, so this ticket tests against a fake.

**Done when:** invalid signatures are rejected; a command from a non-owner ID is ignored and logged; a command in an App-authored comment triggers nothing; a duplicate webhook delivery maps to the same workflow ID (Ticket 8 proves Temporal rejects the second start); an installation token is obtained and used; no token appears in any log or trace.

**Requirements:** FR-I1, FR-SEC1, FR-SEC2, FR-SEC5, FR-SEC15, FR-SEC28, NFR-P1 · **Review list:** RL-7, RL-8

**Manual setup:** register the App (name it at registration; `Conclave` if free, otherwise `Conclave Agent`), grant contents, pull requests, issues, checks, and metadata — never `workflows` — install on the Election Tally repository only, and enable branch protection on its default branch with no App bypass.

---

## Ticket 7 — Gates and artifact integrity

Implement the Milestone 1 gates: build, lint, tests, diff size, secrets (`gitleaks`), protected paths, gate weakening, and execution surface. Implement both gating modes; default `reset_in_place`.

Freeze first: kill sandbox processes, extract the diff, hash it, then gate that recorded artifact. Record the hash on every gate result.

**Done when:** each gate has a fixture that fails it; an attempt that deletes or skips a test is rejected; a diff touching `package.json` scripts, `.husky/`, `.gitattributes`, or `.claude/` is flagged; a gate result's artifact hash matches the diff that was checked; an environment build failure yields `errored`, not `failed_gates`.

**Requirements:** FR-A1, FR-A3, FR-SEC25, FR-SEC27 · **Review list:** RL-2

---

## Ticket 8a — Budgets and attribution

Runs before Ticket 5. Make every model call attributed and bounded by all three FR-SEC9 caps, with no Temporal code: this is `internal/llm` and `internal/store` work only.

**Mandatory attribution.** `llm.Scope` currently requires only `RepoID`, `KeyPrefix`, and `TokenBudget`:
- With `AttemptID` empty, the ledger is never read, so a retried activity starts with a fresh full budget.
- With `FeatureRequestID` empty, cost events get a NULL `feature_request_id`, and the feature request cap can't see that spend.

Require both on any scope that carries a budget. Check once, when the scope is created, that the attempt belongs to the feature request. A call made outside an attempt gets its own explicitly named constructor, which still requires the feature request ID.

**Reservations across scopes.** An activity abandoned after a heartbeat timeout can keep running while its retry opens a second scope, and a ledger re-read doesn't see a call that is still in flight. So, before each model call, one short transaction:
- takes `pg_advisory_xact_lock` on a single global budget key
- sums settled spend from `cost_events` plus live reservations, for the attempt, the feature request, and the UTC calendar month
- refuses if any cap would be passed, otherwise inserts a reservation

After the reply, one transaction records the cost event and deletes the reservation. Reservations expire on the database clock (adapter timeout plus a margin). Migration 00005 adds `budget_reservations` (`conclave_app` gets SELECT, INSERT and DELETE, no UPDATE) and an index on `cost_events (occurred_at)`.

**Caps.**
- The per-attempt budget stays.
- A per-feature-request cap in tokens, summed over every attempt of the feature request.
- A monthly cap in USD, summed from `cost_usd` across all repositories for the calendar month in UTC, computed in SQL.

Cap values come from owner configuration in `deploy/router.yaml`, validated at startup, with placeholders that fail validation. Each refusal is a `BudgetError{Kind}` (`attempt`, `feature_request`, `month`) that unwraps to the existing `ErrBudgetExceeded`, so `IsPermanent` holds and no new sentinel is added.

**Done when:** a budgeted scope without an attempt ID or feature request ID, or with an attempt that doesn't belong to its feature request, is rejected; two concurrent scopes for the same attempt cannot together spend more than its budget, even with one call unsettled; concurrent attempts cannot together pass the feature request or monthly cap; feature request and monthly cap refusals happen before a provider is reached; the month boundary is UTC and tested on both sides; an expired reservation stops counting; each refusal satisfies `errors.Is(err, ErrBudgetExceeded)` and `IsPermanent`, and names its kind; a retried cost-event write records one event; `conclave_app` cannot update a reservation; missing or placeholder cap values fail startup validation.

**Requirements:** FR-SEC9, NFR-M7 · **Review list:** RL-2

**Before starting:** FR-S11, FR-S12, FR-SEC9, FR-SEC31, and threat model D1 now describe a stop as `needs_attention` plus `/retry`, with no `/resume`. See decision record 0009.

---

## Ticket 8-pre — Workflow prerequisites

Everything ticket 8's first activities need and the merged code lacks, with no Temporal code. Full plan: `ticket-8-pre-plan.md`.

Contract batch for hand review: `SandboxProvider.Release(ctx, id)`, `SandboxProvider.CreateWithInstall` lifted from the Docker provider, `ReasonCancelled`, and (proposed) `ReasonBudgetCap`. Docker `Release` by ID, idempotent, refusing unlabelled containers. Store functions for repos (upsert from configuration), feature requests, plans, tasks and attempts, each idempotent on its natural key, plus write-ahead `sandbox_id`, `sandbox_released_at`, an idempotent `artifact_hash` write, `FinishAttempt`, and `UnreleasedAttempts` for the reaper. `deploy/sandboxes.yaml` with image digest, limits, install, env and default branch, validated at startup; a `profile` block in `deploy/toolchains.yaml`. Actor-string helpers for the audit trigger.

**Done when:** `Release` removes a sandbox by ID and is idempotent; every store function has a run-twice test; a feature request and attempt created through the store satisfy `llm.Router.Scope`; the configuration loaders reject each placeholder and bad value with a clear message; `conclave_app` cannot delete work rows; `make check` passes.

**Requirements:** NFR-R2, NFR-R3, FR-SEC12, FR-SEC30, NFR-M7 · **Review list:** RL-2, RL-3, RL-13, RL-14

---

## Ticket 8 — Feature workflow core

The feature workflow: analyze, implement, gate, deliver. Activities for every side effect; deliver is a stub until Ticket 9. Signal handlers for `/approve`, which pins the base SHA, and `/cancel`. Workflow ID derived from the issue number. Deferred cleanup using `workflow.NewDisconnectedContext`.

Minimal routing to `needs_attention`, so that no problem strands the workflow or leaves an automatic failed status. Every non-retryable error, failed attempt, and budget refusal makes a compare-and-set transition to `needs_attention`, with a reason (`budget_cap` with the cap kind, `attempt_failed`, or `error`) and the phase it left. The workflow then waits; only `/cancel` is handled there in this ticket.

Activities build each `llm.Scope` with the attempt ID and the feature request ID, and copy `PromptVersion` from the `RoleSpec`. An `llm.IsPermanent` error becomes `temporal.NewNonRetryableApplicationError`. A `BudgetError` uses the type `budget_cap:<kind>`, because typed errors don't survive the activity boundary. A `RateLimitError` retries after its `RetryAfter`.

Decide the activity granularity. Set `StartToCloseTimeout` above the 10-minute adapter timeout, with a heartbeat, including while the rate limiter blocks. Wire `LoadConfig`, `LoadRepoAccess`, `DirectoryRepos`, the cost ledger, and 8a's budget store into `cmd/worker`.

Milestone 1 statuses only: feature requests use the six-status model; tasks use `running`, `done`, `needs_attention`, `cancelled`; attempts use `running`, `passed`, `failed`, `errored`.

Added 2026-10-26, binding (see `ticket-8-pre-plan.md`, "Carried into ticket 8"): the task phase is a child workflow, `TaskWorkflow`, with serializable inputs and no GitHub call, so Milestone 2's benchmark starts it directly; one attempt is one activity with `MaximumAttempts: 1`; the analyze step parses owner-written issue sections with no model call and posts the plan comment; the deliver stub routes to `needs_attention` rather than `delivered`; config-file digests go into the plan body; the outcome table has a test per row.

**Done when:** `workflowcheck` passes; cancelling the parent cancels the child and still releases the sandbox; no map is ranged over without sorting; the workflow is tested with `testsuite` without a server; a replay test over a history captured mid-attempt finishes without re-running completed activities; a workflow waiting on `/approve` resumes after a long wait using time skipping; a cancellation during the agent phase still releases the sandbox; a `/cancel` during `needs_attention` reaches `cancelled`; a feature request or monthly cap hit moves to `needs_attention` with `reason: budget_cap` naming the cap, keeping its attempts; a non-retryable error moves to `needs_attention` rather than failing the workflow; every activity has an idempotency test that runs it twice and asserts one effect; a permanent router error is not retried.

**Requirements:** NFR-R1, NFR-R2, NFR-R3, FR-S5, FR-S12, FR-S19 (pinning) · **Review list:** RL-1, RL-2, RL-3, RL-13, RL-14, RL-16

**State per activity, in the PR description:** what recovers a death after the outside action but before the Postgres write, write-ahead intent or idempotent retry.

---

## Ticket 8b — Commands and status surface

`/retry` from `needs_attention` returns to the phase the feature request left, so a stop during planning still waits for `/approve`. It keeps every attempt and its recorded cost, and re-checks the caps first; while still over a cap it replies naming the cap and changes nothing.

A command not valid in the current status gets a clear reply and changes nothing, following the `ARCHITECTURE.md` command table. Live statuses are checked in the workflow. `delivered` and `cancelled` are checked in Ticket 6's dispatcher, since their workflow has closed.

`/status` replies where the table allows it. Before starting, decide whether `/help` (in FR-S11, missing from the table) goes into the table, and how `/revise` and `/ask` reply in Milestone 1. Every command, accepted or rejected, is recorded with its actor.

One status comment on the issue, edited in place through Ticket 6's client, names the status, the reason, any cap that was hit, and the commands available.

**Done when:** `/retry` after a raised cap continues in the phase it left, with earlier attempts and cost intact; `/retry` while still over a cap stays in `needs_attention`; `/retry` after a planning-phase stop still waits for `/approve`; each live status rejects the commands it doesn't accept (workflow test per status), and each terminal status does too (dispatcher test per status); the status comment is edited in place, and editing it twice leaves one comment; every command has an audit record.

**Requirements:** FR-S11, FR-S12, FR-SEC10 · **Review list:** RL-9, RL-13

---

## Ticket 9 — Pull request delivery

Minimal `pr_writer` role. Branch model: integration branch pushed after the task lands, `pr-1` cut from it. Commits authored by the App's bot identity, one squashed commit per task, Conventional Commits with `Task`, `Plan`, `Model`, and `Generated-by` trailers. Rebase onto the default branch, rerun gates, then open the PR as a draft; flip to ready when the repository's CI passes.

Record each PR's base SHA. Push refuses any artifact whose hash does not match a passing gate result. The CI result is observed through a mechanism this ticket chooses and records (a `check_suite` or `workflow_run` webhook, or polling the checks API) and written to the PR record; the webhook receiver today knows only `ping`, `issues` and `issue_comment`.

Trimmed 2026-10-26: restacking (FR-S18a) and branch cleanup on merge and after 30 days (FR-S20) are deferred to Milestone 4, since Milestones 2 and 3 never produce a stack. The base SHA is still recorded so restacking needs no migration later. The rebase-and-re-gate step calls `gate.Gate` unchanged.

**Done when:** a PR opens with the described body including the risk section; the push path rejects a tampered artifact; a base SHA is recorded; the CI result is recorded on the PR record and the draft is flipped to ready on success; commits show the bot as author; every run that reaches delivery ends `delivered` and no run before this ticket could.

**Requirements:** FR-S10, FR-S16, FR-S17, FR-S19, FR-A5, FR-A7, FR-SEC27 · **Review list:** RL-2, RL-21

---

## Ticket 10 — End-to-end run and hardening

Split on 2026-10-31 into 10a (the hardening code) and 10b (the live trials), each its own session and pull request. Full plan and owner decisions: `ticket-10-plan.md`.

### Ticket 10a — Kill switch, alerts and scheduled reaper

Add the orphan reaper as a scheduled workflow. Add the kill switch as a `conclave halt` command in a new `cmd/conclave` binary: it records the halt in Postgres, cancels every open workflow, sweeps sandboxes with no age cutoff, and workers stop and refuse to start while it is set; `conclave resume` clears it and cancels runs started during the halt. Add the local append-only alert log and the alert-issue path, delivered to a private operations repository with an issues-only token; Milestone 1's sources are the kill switch (`halt`) and a budget-cap stop (`pause`). Write the trial-report SQL query (resolve rate, cost per resolved task, time per task), which Milestone 2's report generator starts from. Record the deferrals in `m1-exit-checklist.md` item 10 as a decision.

**Done when:** the reaper runs on a Temporal schedule and is safe to run twice; the kill switch cancels everything in flight, releases every sandbox, and stops the workers, proved against a real Temporal server; a worker killed mid-attempt lets a second worker continue the workflow without repeating completed activities (NFR-R1, moved here from Ticket 8, whose testsuite has no workers); a deliberately triggered alert opens an issue, a repeat updates that issue, and both are written to the local log first; the report query is tested against seeded rows; `make check` passes.

**Requirements:** FR-SEC11, FR-SEC31, NFR-R1, NFR-R3, NFR-M9 · **Review list:** RL-18

### Ticket 10b — End-to-end trials

Run the full path against the Election Tally repository ten times, including two cancellations. Fix what breaks. Kill a worker mid-attempt during one trial and continue on a second.

The ten trial issues are written in the owner-section format (Task, Acceptance criteria, Hints) and saved under `docs/trials/local/` (git-ignored, because the target repository is private) with base SHA, attempt, gate results, PR, CI result, cost and duration, so they seed Milestone 2's replay benchmark. The trial report comes from 10a's query.

**Done when:** the 7-of-10 success criterion is met and the trial log exists; no orphaned containers remain; the kill switch is exercised once with a run in flight; a `docs/decisions/` record exists for every non-obvious choice made during the milestone.

**Requirements:** NFR-R1 (live), NFR-R3, NFR-M9 · **Review list:** RL-18

---

## Not in this milestone

Planner, judge, test writer, codebase brief, merge queue, stacked PRs, multiple providers, embeddings, Daytona, multi-machine workers, evaluation harness, canary tokens, prompt injection fixtures, dependency-addition tool, and self-runs on the Conclave repository (`FR-SEC26` forbids these before Milestone 2).
