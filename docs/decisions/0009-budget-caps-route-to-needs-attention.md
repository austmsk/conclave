# 0009 — A stopped feature request goes to needs_attention, and /retry continues it

**Date:** 2026-10-17
**Status:** proposed

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
