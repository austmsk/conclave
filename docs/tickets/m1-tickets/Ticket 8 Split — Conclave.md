# Ticket 8 Split — Conclave

Oct 17, 2026 · @Austin · revised the same day after a three-reviewer pass

## How to use this doc

Ticket 8 in `docs/tickets/milestone-1.md` is split into three sessions: 8a, 8, and 8b. Each runs as its own Claude Code session, per the one-ticket-per-session rule in `CLAUDE.md`.

For each session, paste three things into the prompt: **Shared decisions**, **Session guardrails**, and the section for that ticket. Each ticket section is written to stand alone and replaces the matching part of the original Ticket 8 text.

**Before starting 8a:**

1. `docs/tickets/milestone-1.md` matches this split (done). The repo is the source of truth, and a session that reads the old Ticket 8 will try to do all of it.
2. **Amend the requirements to match the shared decisions** (done; decision record 0009, `proposed`). Five places described a pause and `/resume`:
   - FR-S11 (`scope-and-requirements.md:74`) lists `/resume`
   - FR-S12 (`:75`) says the platform "pauses"
   - FR-SEC9 (`:116`) says hitting a cap "pauses work"
   - FR-SEC31 (`:138`) says a budget alert stops one feature "pending `/resume`"
   - threat model D1 (`security-and-threat-model.md:131`) says the caps "pause work"

   Change each one to: a pause is `needs_attention` with a `reason`, and the owner continues with `/retry`. Drop `/resume` from FR-S11. Write a decision record for the change. A session that reads the unamended requirements will rebuild `/resume`.

## Why split, and the new order

The original Ticket 8 combines three different jobs: the Temporal workflow, budget work carried over from Ticket 4, and GitHub-facing command handling. That leaves the riskiest ticket in Milestone 1 with about ten done-when checks across six review-list items.

The budget work needs no Temporal code, so it moves out and lands earlier. The new order is:

1. Ticket 8a — budgets and attribution
2. Ticket 5 — agent loop and tools
3. Ticket 6 — GitHub App and webhooks
4. Ticket 7 — gates and artifact integrity
5. Ticket 8 — feature workflow core
6. Ticket 8b — commands and status surface
7. Ticket 9 — pull request delivery
8. Ticket 10 — end-to-end run and hardening

8a goes before Ticket 5 for two reasons:
- **Budget errors.** Ticket 5's runner has to tell an attempt budget hit (an agent outcome, `token_limit`) from a feature-request or monthly cap hit (a platform refusal, routed to `needs_attention`). That needs 8a's `BudgetError` kinds. Without them, ticket 5 maps every `ErrBudgetExceeded` to `token_limit` and has to be changed later.
- **Test fixtures.** Ticket 5's test router must pass the IDs that 8a makes mandatory.

The runner itself takes `contracts.Router` and never builds a Scope; the ticket 8 activity does that. So the code coupling is small. `ticket/5` has a branch but no code yet, so run 8a first and rebase `ticket/5` afterwards. 8a also takes migration 00005; ticket 5's `tool_calls` moves to 00006.

The order of 6, 7, 8 is unchanged. Ticket 8's deliver step still precedes Ticket 9, so in Ticket 8 it is an activity stub with an idempotency test, filled in by Ticket 9.

## Shared decisions

These apply to all three tickets and override the original Ticket 8 text where they differ.

- **No `/resume` command and no paused status.** `/resume` is not in the command table in `ARCHITECTURE.md`, and "paused" is not one of the six feature request statuses. Do not add either. The requirements are amended to match (see How to use this doc).
- **A platform cap hit is `needs_attention`.** Hitting the per-feature-request token cap or the monthly USD cap moves the feature request to `needs_attention` with `reason: budget_cap`, recording which cap was hit. The status comment names it.
- **An attempt budget hit is an attempt outcome.** Hitting the per-attempt token budget ends the session with `token_limit` and the attempt as `failed`. The workflow then handles a failed attempt like any other: in Milestone 1 it moves to `needs_attention` with `reason: attempt_failed`, not `budget_cap`.
- **`/retry` resumes, it does not restart.**
  - `/retry` from `needs_attention` returns to the phase the feature request left: `planning` if it left from `planning`, `executing` if it left from `executing`. Both transitions are already legal (`ARCHITECTURE.md:95`, `contracts/state.go:73`). Returning to `executing` after a planning-phase stop would skip `/approve` and break FR-S5.
  - It keeps every existing attempt and its recorded cost. In `executing`, the next attempt is a new ordinal under the same task. It does not create a fresh feature request or discard work.
  - `/retry` re-checks the caps first. If the feature request is still over a cap, it replies naming the cap and stays in `needs_attention`. Cap values come from owner config, read at startup, so the owner raises a cap by editing `deploy/router.yaml` and restarting the worker.
- **Caps are refusals, not failures.**
  - The two new caps reuse the existing `llm.ErrBudgetExceeded`, which `IsPermanent` already classifies as non-retryable. Do not add a new sentinel.
  - Wrap it in a typed error, `llm.BudgetError{Kind}`, where `Kind` is `attempt`, `feature_request` or `month`. `BudgetError` unwraps to `ErrBudgetExceeded` and is returned before any provider is called.
  - **Typed errors don't cross the activity boundary.** The workflow receives a `*temporal.ApplicationError`, where `errors.Is` and `errors.As` no longer work. So the activity converts a `BudgetError` into `temporal.NewNonRetryableApplicationError` with the type string `budget_cap:<kind>`, and the workflow switches on `ApplicationError.Type()`. The workflow treats it as a routing decision, not an error to retry.
- **The cross-scope budget fix uses reservations.** A ledger re-read alone can't meet "two concurrent scopes cannot overspend": it sees only settled spend, and a call still running in an abandoned scope (up to the 10-minute adapter timeout, `anthropic.go:36`) isn't in the ledger yet. A session-level advisory lock doesn't work on a pool either: `DBTX` has only Exec and QueryRow (`store/costs.go:20-23`), and lock and unlock can land on different connections. Holding a dedicated connection across an LLM call would tie up one connection per attempt for minutes. The design, for all three caps at once:
  1. Before each model call, one short transaction:
     - takes `pg_advisory_xact_lock` on a single global budget key (one owner, so contention is negligible, and every cap is serialized by the same lock)
     - sums settled spend from `cost_events` plus live, unexpired reservations, for the attempt, the feature request and the UTC month
     - refuses with the matching `BudgetError` if any cap would be passed
     - otherwise inserts a reservation row
  2. After the reply, one transaction inserts the cost event and deletes the reservation.
  3. A reservation carries `expires_at`, set from the database clock to now() plus the adapter timeout plus a margin. A dead process can't hold budget forever, and an abandoned scope's in-flight call still counts until it expires.

  This holds whether an attempt is one activity or many.

## Ticket 8a — Budgets and attribution

Make every model call attributed and bounded by all three FR-SEC9 caps, with no Temporal code. This is pure `internal/llm` and `internal/store` work, testable without a server.

**Scope**

1. **Mandatory attribution.** `llm.Scope` today requires only `RepoID`, `KeyPrefix`, and `TokenBudget`.
   - With `AttemptID` empty, the ledger is never read, so a retried activity starts with a fresh full budget.
   - With `FeatureRequestID` empty, cost events get a NULL `feature_request_id`, and the feature request cap can't see that spend.
   - Require both on any scope that carries a budget. Check once, when the scope is created, that the attempt belongs to the feature request (attempts → tasks → plans → feature_requests). Otherwise a wrong feature request ID would silently dodge its cap.
   - A model call made outside an attempt (for example during analysis) uses its own explicitly named constructor. It still requires the feature request ID, and is still subject to the feature-request and monthly caps.
2. **Cross-scope budget.** Implement the reservation design in Shared decisions.
   - **Reservation size:** in tokens, the router's existing per-call reservation (input estimate plus `max_tokens`, `router.go:68`). In USD, those tokens priced at the most expensive model in the role's fallback chain.
3. **Per-feature-request token cap.** Settled plus reserved tokens over every attempt of the feature request (and its non-attempt calls), checked in the reservation transaction.
4. **Monthly USD cap.** Settled plus reserved `cost_usd` across all repositories for the current calendar month, checked in the reservation transaction.
   - The month is UTC, computed in SQL (`date_trunc('month', now() AT TIME ZONE 'UTC')`), because the router deliberately reads no clock (`scope.go:168-171`).
5. **Migration 00005.**
   - A `budget_reservations` table: `id`, `attempt_id` (nullable), `feature_request_id`, `tokens`, `cost_usd`, `expires_at`. It can't live in `cost_events`, which is append-only with `CHECK >= 0` (`00003_accounting.sql:22-24`).
   - Grant `conclave_app` SELECT, INSERT and DELETE on it, never UPDATE.
   - Add an index on `cost_events (occurred_at)`. The existing `(repo_id, occurred_at)` index (`00003:29`) can't serve a sum across repositories.
6. **Configuration.** Both cap values come from owner configuration in `deploy/router.yaml`, decoded with the existing `KnownFields(true)` settings and validated at startup. Use placeholders that fail validation, as the file already does for prices.
7. **Errors.** Each refusal is a `BudgetError` naming the kind, per Shared decisions.

**Likely files:** `internal/llm/scope.go`, `internal/llm/config.go`, `internal/llm/cost.go`, `internal/llm/errors.go`, `internal/store/costs.go`, a new `internal/store/budget.go`, `internal/store/migrations/00005_budget_reservations.sql`, `migrate_test.go`, `appendonly_test.go` or a grant test, `deploy/router.yaml`, and their tests. No change to `internal/contracts`.

**Done when**

- [ ] A budgeted scope without an attempt ID or a feature request ID is rejected, and so is one whose attempt doesn't belong to its feature request.
- [ ] Two concurrent scopes for the same attempt cannot together spend more than the attempt's budget, including when one scope's call is still in flight and unsettled.
- [ ] Concurrent calls from different attempts cannot together take the feature request or the month past its cap.
- [ ] A call that would take the feature request past its token cap is refused before reaching a provider.
- [ ] A call that would take the month past its USD cap is refused before reaching a provider. The month boundary is UTC, and a test covers spend on both sides of it.
- [ ] An expired reservation stops counting. A settled call leaves a cost event and no reservation.
- [ ] Each refusal satisfies `errors.Is(err, ErrBudgetExceeded)` and `IsPermanent`, and `errors.As` yields a `BudgetError` with the right kind.
- [ ] A retried cost-event write records the event once. (A retried *call* gets a new reply ID and is real spend. That is correct, and not covered here.)
- [ ] `conclave_app` cannot UPDATE a reservation.
- [ ] Missing or placeholder cap values fail startup validation with a clear message.
- [ ] `make check` passes.

**Requirements:** FR-SEC9, NFR-M7 · **Review list:** RL-2 (self-check 3)

## Ticket 8 — Feature workflow core

Build the feature workflow and prove it survives crashes, retries, and cancellation. Budgets are already done in 8a. This ticket carries the minimal routing to `needs_attention`, so that a problem never strands the workflow before 8b exists.

**Scope**

1. **The workflow.** Analyze, implement, gate, deliver, with every side effect in an activity under `internal/activity/`. Workflow ID derived from the issue number, so duplicate webhooks deduplicate. Deliver is a stub activity until Ticket 9.
2. **Two signals.** `/approve`, which pins the base SHA through `store.ApprovePlan`, and `/cancel`. Both test Temporal's signal mechanics, which is why they stay here.
3. **Cleanup.** Deferred sandbox release using `workflow.NewDisconnectedContext`, so it runs after cancellation.
4. **Minimal routing to `needs_attention`.** Without this, a non-retryable error fails the Temporal workflow and leaves the database row in `planning` or `executing` with no comment, which breaks "no automatic failed status" (`ARCHITECTURE.md:99`). Every non-retryable error, failed attempt and budget refusal makes a compare-and-set transition to `needs_attention` with a `reason`:
   - `budget_cap`, with the cap kind from the `budget_cap:<kind>` error type
   - `attempt_failed`
   - `error`

   The phase it left is recorded. The workflow then waits for a signal. In this ticket only `/cancel` is handled there; `/retry` and the status comment are 8b.
5. **Model calls.**
   - Activities build each `llm.Scope` with the attempt ID and the feature request ID, and copy `PromptVersion` from the role's `RoleSpec` (decision 0006).
   - An error where `llm.IsPermanent` is true is returned as `temporal.NewNonRetryableApplicationError`. A `BudgetError` uses the type string `budget_cap:<kind>`.
   - A `RateLimitError` is returned as a retryable `ApplicationError` whose `NextRetryDelay` is its `RetryAfter`.
6. **Activity shape and timeouts.**
   - Decide the activity granularity, and record it in a decision record. Ticket 4's recommendation was one model call per activity, with the transcript stored outside workflow history.
   - Set `StartToCloseTimeout` above the 10-minute adapter timeout, and set a `HeartbeatTimeout`.
   - Heartbeat while waiting on the router's rate limiter, which today blocks without one.
7. **Worker wiring.** `cmd/worker` loads `LoadConfig`, `LoadRepoAccess`, `DirectoryRepos`, the `CostLedger` and 8a's budget store, validating each at startup.
8. **Statuses.** Milestone 1 statuses only. Feature requests use the six-status model; tasks use `running`, `done`, `needs_attention`, `cancelled`; attempts use `running`, `passed`, `failed`, `errored`.

**Out of scope:** `/retry`, the status comment, command rejection replies, `/status` and `/help`. Those are 8b.

**Done when**

- [ ] `workflowcheck` passes, and no map is ranged over without sorting.
- [ ] The workflow is tested with `testsuite`, without a server.
- [ ] **Replay instead of a crash test.** A replay test (`worker.WorkflowReplayer`) over a captured history, stopped mid-attempt, finishes without re-running any completed activity. testsuite has no workers, so "resumes on another worker" can't be tested here; the real two-worker kill test is in Ticket 10.
- [ ] A workflow waiting on `/approve` resumes correctly after a long wait, using the test suite's time skipping.
- [ ] A cancellation during the agent phase still releases the sandbox.
- [ ] A `/cancel` during `needs_attention` reaches `cancelled` and still releases any sandbox.
- [ ] A feature request that reaches its token cap moves to `needs_attention` with `reason: budget_cap` naming the feature request cap, and keeps its attempts. A monthly cap refusal does the same, naming the monthly cap.
- [ ] A non-retryable error moves the feature request to `needs_attention` instead of failing the workflow.
- [ ] Every activity has an idempotency test that runs it twice and asserts one effect.
- [ ] A permanent router error is not retried. A rate-limit error is retried after its `RetryAfter`.
- [ ] `make check` passes.

**Review check (from you).** For each activity: if it dies after the outside action (GitHub, Docker) but before the Postgres write, what recovers the gap, a Temporal retry or a sweeper? Each activity needs one clear answer, either write-ahead intent or idempotent retry (RL-2, RL-14). Ask the session to state it per activity in the PR description.

**Requirements:** NFR-R1, NFR-R2, NFR-R3, FR-S5, FR-S12 (routing), FR-S19 (pinning only; the rebase half is Ticket 9) · **Review list:** RL-1, RL-2, RL-3, RL-13, RL-14, RL-16

## Ticket 8b — Commands and status surface

Let the owner continue with `/retry`, reject commands that don't fit the current status, and show the current state on the issue. This ticket owns the feature request lifecycle as the owner sees it (RL-9). Routing into `needs_attention` already exists from Ticket 8.

**Scope**

1. **`/retry`.** Per Shared decisions: it returns to the phase the feature request left, keeps existing attempts and their recorded cost, and re-checks the caps first.
2. **Invalid commands.** A command that is not valid in the current status gets a clear reply and changes nothing, following the command table in `ARCHITECTURE.md`.
   - **Live statuses** (`planning`, `awaiting_approval`, `executing`, `needs_attention`) are checked in the workflow's signal handling.
   - **Terminal statuses** (`delivered`, `cancelled`): the workflow has closed and can't receive a signal, so these are checked in Ticket 6's webhook dispatcher, which 8b extends.
   - `/revise` and `/ask` are in the command table, but no Milestone 1 ticket implements them (Milestone 1 has no planner). Proposal, to confirm before 8b: they reply that they are not available in Milestone 1, and change nothing.
3. **`/status` and `/help`.** FR-S11 lists both, and no earlier ticket owns them.
   - `/status` replies with the current status, phase and reason, in the statuses where the command table allows it (`executing` today).
   - `/help` lists the commands valid in the current status. It is in FR-S11 but missing from the `ARCHITECTURE.md` command table. Decide before 8b starts whether to add it to the table in every live status. Until the table has it, the session treats it as a new command and stops, per the guardrails.
4. **Status comment.** One comment on the issue, updated in place as the status changes, not a new comment per change. It uses Ticket 6's client, which owns comment posting and in-place editing (`milestone-1.md`, Ticket 6). When the status is `needs_attention`, the comment names the reason, the cap if there is one, and the commands available.
5. **Audit.** Every command received, whether accepted or rejected, is recorded with its actor and timestamp (FR-SEC10).

**Done when**

- [ ] `/retry` after a feature-request cap hit, once the cap is raised, continues the run in the phase it left. The attempts and cost recorded before the cap are still there.
- [ ] `/retry` while still over a cap replies naming the cap and stays in `needs_attention`.
- [ ] `/retry` after a stop during `planning` returns to `planning`, and implementation still waits for `/approve`.
- [ ] Each of the four live statuses rejects the commands it does not accept, with a workflow test per status. Each of the two terminal statuses rejects commands in the dispatcher, with a test per status.
- [ ] `/status` replies correctly wherever the command table allows it, and `/help` does too once it is in the table.
- [ ] The status comment is edited in place, and editing it twice leaves one comment.
- [ ] Every command, accepted or rejected, has an audit record.
- [ ] `make check` passes.

**Requirements:** FR-S11, FR-S12, FR-SEC10 · **Review list:** RL-9, RL-13

## Ticket 4 carry-overs

Every item on Ticket 4's "revisit at ticket 8" list, and where it lands.

| Item | Where |
|---|---|
| Two live scopes on one attempt can spend twice the budget | 8a, item 2 (reservations) |
| A scope without an `AttemptID` has no budget protection across retries | 8a, item 1 |
| Per-feature-request and per-month caps | 8a, items 3 and 4 |
| Wrap `IsPermanent` errors as non-retryable | 8, item 5 |
| `ErrBudgetExceeded` as a pause (FR-SEC9) | Shared decisions (`needs_attention`); 8, item 4 |
| Temporal SDK not in `go.mod` | 8 (guardrail; decision record owed) |
| Wire `DirectoryRepos`, `LoadRepoAccess`, `LoadConfig`, `CostLedger` into `cmd/worker` | 8, item 7 |
| `RateLimitError.RetryAfter` as the retry interval | 8, item 5 |
| 10-minute adapter timeout against the activity timeouts | 8, item 6 |
| Activity granularity | 8, item 6 (decision record) |
| The limiter blocks without heartbeating | 8, item 6 |
| Embedding cost keys are random without a reply ID | Milestone 2 (embeddings aren't in Milestone 1) |

## Session guardrails

The session should stop and ask, not proceed, in these cases. They restate rules from `CLAUDE.md` that each of these tickets is likely to hit.

- **New dependency.** Ticket 8 is the first to need the Temporal Go SDK, which is not in `go.mod` yet. Propose it and wait; it also needs a decision record under NFR-M4.
  - Ticket 6 must not add the SDK. It dispatches through a small workflow-starter interface with a fake, and proves that duplicate deliveries map to the same workflow ID. Ticket 8 proves Temporal rejects the second start.
- **Contract change.** Any change to `internal/contracts` is described and stopped on, not made. None of the three tickets should need one.
- **Scope creep across the split.**
  - If 8a starts touching workflow code, stop and say so.
  - If Ticket 8 starts on `/retry`, the status comment, or command rejection replies, stop and say so.
  - If 8b changes routing into `needs_attention` beyond adding a reason, stop and say so.
- **A new status or command.** Do not add `/resume`, a paused status, or a failed status. If something seems to need one, describe the case and stop.
- **Circuit breaker.** After two failed attempts at the same error, stop and report what was tried.

After each merge, write the decision records yourself:
- **Before 8a:** the requirements amendment (pause becomes `needs_attention` plus `/retry`).
- **8a:** the reservation design (global transaction lock, DB-clock expiry, UTC month), and why a re-read or a session lock wasn't enough.
- **8:** the Temporal SDK dependency, activity granularity, and the timeout values.
