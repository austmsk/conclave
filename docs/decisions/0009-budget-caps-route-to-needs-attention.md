# 0009 — A stopped feature request goes to needs_attention, and /retry continues it

**Date:** 2026-10-17
**Status:** accepted

## Context

The requirements and the architecture described a stopped feature request in two different ways.

- **Requirements: pause and `/resume`.**
  - FR-SEC9 said hitting a cap "pauses work".
  - FR-S12 said a stuck platform "pauses and posts a comment".
  - FR-S11 listed `/resume` alongside `/retry`.
  - FR-SEC31's `pause` alert stopped a feature "pending `/resume`".
  - Threat model D1 described caps "that pause work".
  - The original Ticket 8 followed these: it said to wait for `/resume` when a cap was hit.
- **`ARCHITECTURE.md`: six statuses and no pause.** It has six feature request statuses, and "paused" isn't one of them. Its command table has no `/resume`. It says "there is no automatic `failed` status: every problem routes to `needs_attention` with a `reason`, and the owner decides." From `needs_attention`, `/retry` moves to `executing` or `planning`.

Building it as written would have meant one of two things:
- **A seventh status** (`paused`). The transition table from ticket 2, its exhaustive test, the compare-and-set helper and the state transition trigger would all have had to change, along with `internal/contracts`, which is reviewed by hand.
- **A pause hidden inside `needs_attention`, with its own command.** Two commands, `/retry` and `/resume`, would then do almost the same thing from the same status, and the owner would have to know which one a given stop expected.

I noticed the contradiction while splitting Ticket 8 into 8a, 8 and 8b. Budget enforcement (8a) is the first code that has to decide what a cap hit does.

## Decision

`ARCHITECTURE.md` wins, and the requirements were amended to match:

- **Every stop is `needs_attention` with a `reason`.** That covers a stuck platform, a failed attempt, an error, and a cap hit. A feature-request or monthly cap gives `reason: budget_cap`, naming which cap was hit. There is no paused status and no automatic failed status.
- **An attempt budget hit is an attempt outcome, not a platform stop.** The session ends with `token_limit`, and the attempt is `failed`. The feature request then reaches `needs_attention` the way any failed attempt does, with `reason: attempt_failed`.
- **`/retry` is the only way to continue, and it resumes rather than restarts.**
  - It returns to the phase the feature request left: `planning` or `executing`. A stop during planning can't skip `/approve` (FR-S5).
  - It keeps every attempt and its recorded cost.
  - It re-checks the caps first. While still over a cap, it replies naming the cap and changes nothing. The owner raises a cap by editing `deploy/router.yaml` and restarting the worker.
- **`/resume` is removed** from FR-S11.

The amended requirements are FR-S11, FR-S12, FR-SEC9 and FR-SEC31, plus threat model D1. FR-SEC31 keeps the alert severity name `pause`, which now means "moves one feature request to `needs_attention` pending `/retry`".

## Consequences

- No contract or schema change. The six statuses, the transition table and its tests stay as ticket 2 built them, and both phase-return transitions are already legal.
- The owner learns one rule: anything that needs them shows up as `needs_attention`, and `/retry` continues it. The status comment carries the difference between a budget stop and a failed attempt, through the `reason`.
- `needs_attention` now covers different kinds of stop. To pick the right action, the workflow must remember which phase it left, and the comment must say why it stopped. A `/retry` after `budget_cap` that is still over the cap is refused, not looped.
- Raising a cap needs a worker restart, because cap values are read and validated at startup. This is acceptable for a single owner, but it is friction. Reloading config live would be a separate decision.
- The alert severity is still called `pause` while the status it produces is `needs_attention`. I kept the name so Ticket 10's alert work doesn't change, though the mismatch reads a little oddly.
- FR-S11 still lists `/help`, but `ARCHITECTURE.md`'s command table doesn't. That gap is unrelated to this record, and gets decided before Ticket 8b.

## Amendment, 2026-11-21: what closed, how a cap stop ends without an owner, and one monthly cap for live work and the benchmark

Reviewed after Milestone 1 closed. The decision held: one rule for the owner, no contract or schema change.

**What closed.**
- Ticket 8b built `/retry` as described: it continues in the phase left, keeps every attempt and its cost, and re-checks the caps first (`TestRetryAfterCapRaised`, `TestRetryStillOverCap`, `TestRetryFromPlanningWaitsForApprove`).
- Ticket 10a raises the `pause` alert on a cap stop (`TestBudgetCapRaisesPauseAlert`).
- The `/help` gap noted above was settled by ticket 8b and decision 0013.
- The reason is written `budget_cap:<kind>`, with the cap's kind after the colon.
- Raising a cap still means editing `router.yaml` and restarting the worker.

**The command is being renamed.** The owner found "retry" misleading, since it continues rather than starts over. The ticket in `docs/tickets/m2-tickets/continue-command.md` makes `/continue` the name the owner types and reads, keeps `/retry` as an alias, and keeps `retry` as the internal signal and audit value so waiting runs and replay are unaffected. Until it lands, `/retry` is the command.

**A cap stop with no owner.** The wait for the owner belongs to `FeatureWorkflow`, not to the task. On a cap stop, `TaskWorkflow` moves its task to `needs_attention`, returns outcome `TaskBudgetCap` with the cap's kind, and ends. The pause alert is raised only by the parent. So Milestone 2's benchmark, which runs `TaskWorkflow` without a parent, gets a final result it can record as `budget_exhausted`, and raises no alerts. Nothing in this decision changes for it.

The attempt-budget rule carries over the same way. An attempt that runs out of its own tokens is `failed` (`token_limit`), a model result, scored as unresolved. A feature-request or monthly cap stop is a platform limit, scored as `budget_exhausted` and not counted against the model.

Two consequences for the benchmark runner:
- **Its tasks stay in `needs_attention`.** No one continues a benchmark task, so the runner moves each cap-stopped task to a final state when it records the result, or every query that does not tell benchmark rows from live ones will show them as stuck work.
- **Its spend is attributed like any other.** Every model call is a `cost_events` row with its feature request and attempt, so a report can separate benchmark spend from live spend by the rows the runner seeded.

**One monthly cap, shared by live work and the benchmark.** Decided by the owner on 2026-11-21. The monthly cap stays a single dollar total across every repository and every kind of run, and the benchmark gets no separate allowance.
- **Why:** it is the one number that bounds the month's spend, whatever is running. A second allowance would mean two numbers to keep in step, and a total no single cap enforces.
- **The cost of it:** a large benchmark run and live work draw on the same money. The shipped cap is $200 a month, and the ten trials spent $51.41, about a quarter of it. Milestone 2's official runs (tasks × 3 seeds × 2 models, plus the SWE-bench subset) would come to hundreds of attempts at the trials' $5 to $7 each, far over $200. Before an official run, the owner raises the cap for that month, and the per-run cap in the Milestone 2 plan bounds the run itself, so one run cannot use the whole month by itself.
- **Live work during a run.** While a benchmark run is in flight, a live feature request can stop on the monthly cap because of benchmark spend. It goes to `needs_attention` with `budget_cap:month` like any cap stop, and continues once the cap is raised or the month turns. The owner times official runs for when live work can wait.

