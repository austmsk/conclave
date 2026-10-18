# Ticket 8 Split — Conclave

Oct 17, 2026 · @Austin

## How to use this doc

Ticket 8 in `docs/tickets/milestone-1.md` is split into three sessions: 8a, 8, and 8b. Each runs as its own Claude Code session, per the one-ticket-per-session rule in `CLAUDE.md`.

For each session, paste three things into the prompt: **Shared decisions**, **Session guardrails**, and the section for that ticket. Each ticket section is written to stand alone and replaces the matching part of the original Ticket 8 text.

Before starting, update `docs/tickets/milestone-1.md` to match this split. The repo is the source of truth, and a session that reads the old Ticket 8 will try to do all of it.

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

8a goes before Ticket 5 because the agent loop is the first real caller of `llm.Scope`. If attribution becomes mandatory after Ticket 5, every call site Ticket 5 creates has to be retrofitted. If you're already past Ticket 5, run 8a immediately before Ticket 8 instead.

## Shared decisions

These apply to all three tickets and override the original Ticket 8 text where they differ.

- **No `/resume` command and no paused status.** `/resume` is not in the command table in `ARCHITECTURE.md`, and "paused" is not one of the six feature request statuses. Do not add either.
- **A cap hit is `needs_attention`.** Hitting the per-feature-request token cap or the monthly USD cap moves the feature request to `needs_attention` with `reason: budget_cap`. The status comment names which cap was hit.
- **`/retry` resumes, it does not restart.** `/retry` from `needs_attention` continues the feature request and keeps every existing attempt and its recorded cost. It does not create a fresh feature request or discard work.
- **Caps are refusals, not failures.** The two new caps reuse the existing `llm.ErrBudgetExceeded`, which `IsPermanent` already classifies as non-retryable. Wrap it so callers can tell which budget was hit (attempt, feature request, or month), and return it before any provider is called. Do not add a new sentinel. The workflow treats it as a routing decision, not an error to retry.
- **The cross-scope budget fix does not depend on activity granularity.** A Postgres advisory lock keyed on the attempt ID, or a ledger re-read before each call, holds whether an attempt is one activity or many.

## Ticket 8a — Budgets and attribution

Make every model call attributed and bounded by all three FR-SEC9 caps, with no Temporal code. This is pure `internal/llm` and `internal/store` work, testable without a server.

**Scope**

1. **Mandatory attribution.** `llm.Scope` today requires only `RepoID`, `KeyPrefix`, and `TokenBudget`. With `AttemptID` empty the ledger is never read, so a retried activity starts with a fresh full budget. With `FeatureRequestID` empty, cost events get a NULL `feature_request_id` and the feature request cap cannot see that spend. Require both on any scope that carries a budget. A deliberately unattributed call gets its own explicitly named constructor.
2. **Cross-scope attempt budget.** A scope seeds its spend from the ledger once, then tracks it in memory. An activity abandoned after a heartbeat timeout can keep running while its retry opens a second scope, so the two together can spend up to twice the budget. Make the check hold across scopes: either a Postgres advisory lock keyed on the attempt ID, or a ledger re-read before each call. Pick one, and record why in a decision record.
3. **Per-feature-request token cap.** Summed over every attempt of the feature request, read from the cost ledger, checked before each model call.
4. **Monthly USD cap.** Summed from `cost_usd` across all repositories for the calendar month, read from the cost ledger, checked before each model call.
5. **Configuration.** Both cap values come from owner configuration in `deploy/router.yaml`, decoded with the existing `KnownFields(true)` settings and validated at startup. Use placeholders that fail validation, as the file already does for prices.
6. **Errors.** Each cap returns a wrapped `ErrBudgetExceeded` that names the budget hit, per Shared decisions.

**Likely files:** `internal/llm/scope.go`, `internal/llm/config.go`, `internal/llm/cost.go`, `internal/store/costs.go`, `deploy/router.yaml`, and their tests. No change to `internal/contracts`.

**Done when**

- [ ] A budgeted scope without an attempt ID or a feature request ID is rejected.
- [ ] Two concurrent scopes for the same attempt cannot together spend more than the attempt's budget.
- [ ] A call that would take the feature request past its token cap is refused before reaching a provider.
- [ ] A call that would take the month past its USD cap is refused before reaching a provider.
- [ ] Each refusal satisfies `errors.Is(err, ErrBudgetExceeded)` and `IsPermanent`, and says which budget was hit.
- [ ] A cost event is recorded once even when the call that produced it is retried.
- [ ] Missing or placeholder cap values fail startup validation with a clear message.
- [ ] `make check` passes.

**Requirements:** FR-SEC9, FR-SEC10, NFR-M7 · **Review list:** RL-2 (self-check 3), RL-13

## Ticket 8 — Feature workflow core

Build the feature workflow and prove it survives crashes, retries, and cancellation. Everything in this ticket tests Temporal behavior; budgets are already done in 8a.

**Scope**

1. **The workflow.** Analyze, implement, gate, deliver, with every side effect in an activity under `internal/activity/`. Workflow ID derived from the issue number, so duplicate webhooks deduplicate.
2. **Two signals.** `/approve`, which pins the base SHA through `store.ApprovePlan`, and `/cancel`. Both test Temporal's signal mechanics, which is why they stay here.
3. **Cleanup.** Deferred sandbox release using `workflow.NewDisconnectedContext`, so it runs after cancellation.
4. **Model calls.** Activities build each `llm.Scope` with the attempt ID and feature request ID, and copy `PromptVersion` from the role's `RoleSpec` (decision 0006). An error where `llm.IsPermanent` is true is returned as `temporal.NewNonRetryableApplicationError`.
5. **Statuses.** Milestone 1 statuses only. Feature requests use the six-status model; tasks use `running`, `done`, `needs_attention`, `cancelled`; attempts use `running`, `passed`, `failed`, `errored`.

**Out of scope:** `/retry`, routing to `needs_attention`, and the status comment. Those are 8b.

**Done when**

- [ ] `workflowcheck` passes, and no map is ranged over without sorting.
- [ ] The workflow is tested with `testsuite`, without a server.
- [ ] A simulated worker crash mid-attempt resumes on another worker without repeating completed steps.
- [ ] A workflow waiting on `/approve` resumes correctly after a long wait, using the test suite's time skipping.
- [ ] A cancellation during the agent phase still releases the sandbox.
- [ ] Every activity has an idempotency test that runs it twice and asserts one effect.
- [ ] A permanent router error is not retried.
- [ ] `make check` passes.

**Review check (from you).** For each activity: if it dies after the outside action (GitHub, Docker) but before the Postgres write, what recovers the gap, a Temporal retry or a sweeper? Each activity needs one clear answer, either write-ahead intent or idempotent retry (RL-2, RL-14). Ask the session to state it per activity in the PR description.

**Requirements:** NFR-R1, NFR-R2, NFR-R3, FR-S19 · **Review list:** RL-1, RL-2, RL-3, RL-14, RL-16

## Ticket 8b — Commands and status surface

Route every problem to `needs_attention` with a reason, let the owner continue with `/retry`, and show the current state on the issue. This ticket owns the feature request lifecycle (RL-9).

**Scope**

1. **Routing to `needs_attention`.** Every problem the workflow can hit lands in `needs_attention` with a `reason`, never an automatic failed status. A wrapped `ErrBudgetExceeded` from 8a becomes `reason: budget_cap`, naming which cap was hit.
2. **`/retry`.** Leaves `needs_attention` and continues the feature request, keeping existing attempts and their recorded cost, per Shared decisions.
3. **Invalid commands.** A command that is not valid in the current status gets a clear reply and changes nothing, following the command table in `ARCHITECTURE.md`.
4. **Status comment.** One comment on the issue, updated in place as the status changes, not a new comment per change. This could move to Ticket 9 instead, since both are GitHub writes and Ticket 9 owns that client.

**Done when**

- [ ] A feature request that reaches its token cap moves to `needs_attention` with `reason: budget_cap` and keeps its attempts.
- [ ] `/retry` from that state continues the run, and the attempts recorded before the cap are still there.
- [ ] A call refused by the monthly USD cap produces the same routing, naming the monthly cap.
- [ ] Every status in the six-status model rejects the commands it does not accept, with a test per status.
- [ ] A `/cancel` arriving during `needs_attention` or `executing` reaches `cancelled` and still releases the sandbox.
- [ ] The status comment is edited in place, and editing it twice leaves one comment.
- [ ] `make check` passes.

**Requirements:** FR-S5, FR-S11, FR-S12 · **Review list:** RL-9, RL-13

## Session guardrails

The session should stop and ask, not proceed, in these cases. They restate rules from `CLAUDE.md` that each of these tickets is likely to hit.

- **New dependency.** Ticket 8 is the first to need the Temporal Go SDK, which is not in `go.mod` yet. Propose it and wait; it also needs a decision record under NFR-M4.
- **Contract change.** Any change to `internal/contracts` is described and stopped on, not made. None of the three tickets should need one.
- **Scope creep across the split.** If 8a starts touching workflow code, or Ticket 8 starts on `/retry` or the status comment, stop and say so.
- **A new status or command.** Do not add `/resume`, a paused status, or a failed status. If something seems to need one, describe the case and stop.
- **Circuit breaker.** After two failed attempts at the same error, stop and report what was tried.

After each merge, write the decision record yourself. For 8a, that's the advisory lock versus ledger re-read choice. For 8, it's the Temporal SDK dependency.
