# 0016 — What Milestone 1 defers, and the known gaps it leaves

**Date:** 2026-10-31
**Status:** proposed

## Context

Milestone 1 claims a set of requirements (`m1-exit-checklist.md`), and several others that the specification places early were moved later while the milestone was built. Item 10 of the checklist asks that each be recorded as a decision, "so a later session does not treat them as bugs". Some were already recorded in a ticket's own record; this one gathers them in one place and adds those ticket 10a made. Separately, the ticket 9 review left findings the owner chose not to fix. Those are gaps, not choices about scope, and are listed apart so they are not mistaken for either.

## Decision

### Deferred, with where each lands

| Requirement | What is deferred | Why | Lands in |
|---|---|---|---|
| FR-S18a | Restacking PRs above a merged one | Milestones 2 and 3 never produce a stack. The base SHA is already recorded write-once at branch creation, so restacking needs no migration (0014). | Milestone 4 |
| FR-S20 | Branch cleanup on merge, and the 30-day `agent/` branch reaper | Nothing merges automatically in Milestone 1, and the owner deletes branches by hand. A leftover `agent/<n>/integration` stops a new run on the same issue with reason `error`. | Milestone 4 |
| FR-S19 (rerun) | Rerunning a task after a finalization conflict | Rerunning needs a new base pinned for the plan, which is write-once by design. A conflict stops in `rebase_conflict` instead (0014). | Milestone 4 |
| FR-S18, FR-S17 (signed) | Stacked PRs, and the API commit path for repositories that require signed commits | One task, one PR, in Milestone 1. ETS does not require signed commits (0014). | Milestone 4 |
| FR-I5 (tracing) | OpenTelemetry export | Runs are traced through the Temporal UI and the database (`TestTraceableFromWorkflowID`). Jaeger runs in Compose but nothing exports to it. | After Milestone 1 |
| FR-SEC17 | Pre-run secret and personal-data scans before content reaches a model | The gitleaks gate scans the agent's output, not the input. ETS is the owner's own private repository. | After Milestone 1 |
| FR-SEC20 | Canary tokens and their detector | The detector and prompt-injection fixtures belong with the evaluation harness. Tests use the `canary_*` format already. | Milestone 2 |
| FR-SEC22 | Excluded-path substitution | No Milestone 1 repository has excluded paths. | After Milestone 1 |
| FR-SEC30 (build) | CI build of sandbox images, the weekly rebuild and scan, and GHCR publication | The java21 image is built locally and pinned by digest. Workers already pull by digest only, enforced at startup (0011). | After Milestone 1 |
| FR-SEC31 (auto-halt) | Halting on "repeated authorization failure from an unknown actor" | No threshold is specified. The webhook server, which sees the failures, has no database access to record a halt (0015). Refused commands and labels are logged by the receiver. | After Milestone 1 |
| FR-SEC31 (digest) | Batching `digest` alerts weekly | Milestone 1's only digest alert is `conclave alert-test`, delivered at once. | After Milestone 1 |
| FR-SEC31 (canary) | The canary-hit `halt` alert | It needs FR-SEC20's detector. | Milestone 2 |
| FR-SEC31 (channel) | A second alert channel beyond GitHub | Already an accepted residual risk. The local log holds every alert when GitHub is down or suspect. | Not planned |

### Known gaps from the ticket 9 review

These were found in review and left unfixed by the owner's choice on 2026-10-31. Each is a defect or a missing test, not a deferral. A later ticket that touches the code should fix it rather than build on it.

- **Skipped and neutral checks count as passing CI.** `evaluateChecks` treats `skipped` and `neutral` conclusions on a required check as a pass (FR-A7).
- **An empty risks section is allowed.** The pr_writer may return no risks, and the PR body then has an empty section (FR-S10).
- **`WritePullRequest` has no `MaximumAttempts`.** It retries until its schedule-to-close bound, and each retry before the row exists may call the model again. The caps still bound the spend.
- **`DeliveryInput.TaskID` is unused.**
- **The GraphQL URL is built from the REST base URL**, which is wrong on GitHub Enterprise Server. Only github.com is supported in Milestone 1.
- **`minPollEvery` is defined twice**, in the gate policy and in the workflow.
- **No test covers the pr_writer's budget-cap stop** in delivery. Ticket 10a's `RaiseAlert` call covers both cap paths through `needsAttention`, but only the task path has a workflow test.

## Consequences

- A session that finds one of these behaviours should check this record before treating it as a bug.
- The Milestone 2 tickets that pick up FR-SEC20 and the canary alert, and the Milestone 4 tickets for FR-S18a, FR-S19 and FR-S20, start from the seams listed in `m1-exit-checklist.md`.
- Decisions 0002 and 0003 (ticket 2) are still owed by the owner. NFR-M9 is not met for Milestone 1 until they are written.
