# Ticket 10b — End-to-end trials

**Status:** in review · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-10b--end-to-end-trials), [`ticket-10-plan.md`](../../tickets/ticket-10-plan.md#10b--live-trials-next-session-outline-only) · **Requirements:** NFR-R1 (live), NFR-R3, NFR-M9 · **Review list:** RL-18 · **Decisions:** [0016](../../decisions/0016-milestone-1-deferrals.md#found-in-the-ticket-10b-trials) · **Trials:** [`docs/trials/README.md`](../../trials/README.md)

## In one paragraph

Milestone 1's whole path was run for real, ten times, against the private Election Tally repository (ETS) on 2026-11-16. Each trial was one labelled issue, run through `/approve`, an attempt in a sandbox, the gates, a draft pull request, the repository's CI and the flip to ready. **7 of 10 trials delivered a pull request that passed CI**, which meets the milestone's criterion. The 2 cancellations were 2 of the 10 by design. The last trial stopped twice on the agent's wall clock and is unresolved. The run also exercised three drills: `/cancel` mid-attempt, `kill -9` of the worker mid-attempt, and `conclave halt` with a run in flight. No sandbox was orphaned. Two platform defects surfaced and were fixed, each with a test: the agent was never told the diff cap its gate enforced, and a criterion with nested bullets was split into several.

## Why it exists

Every earlier ticket proved its piece with fakes, testcontainers or a test Temporal server. The milestone's success criteria can only be met live:

- a labelled issue becomes a CI-passing PR in 7 of 10 trials;
- no sandbox is orphaned over 10 runs that include 2 cancellations.

NFR-R1 asks that a worker crash never loses work. Ticket 10a proved it against a real Temporal server with fake activities (`TestWorkerKilledMidAttemptResumesOnSecond`), and 10b repeats it with a real worker, a real sandbox and a real model. FR-SEC11's kill switch was likewise proved in 10a with stand-in workflows and is used here on a live run.

RL-18 asks that the controls a compromise would target don't depend on the thing compromised. The halt drill shows it in practice. The halt was recorded in Postgres before anything else happened, the alert went to the private ops repository with its issues-only token, and the workers stopped themselves after reading Postgres. The webhook server, which holds no database access, played no part.

## How it works

### The setup

The owner ran the stack locally:

- `make dev` for Postgres, Temporal and Jaeger;
- `cmd/server` behind a `cloudflared` tunnel ([`docs/trials/webhook-tunnel.md`](../../trials/webhook-tunnel.md));
- `cmd/worker` with the `deploy/local/*` configuration.

Pre-flight checked the ETS ruleset (no bypass actors), the `conclave-reaper` schedule, the pinned sandbox image and `conclave alert-test`. The image check passed even though the local registry was not running: the daemon already held the digest-pinned image, and `ensureImage` pulls only when `ImageInspect` reports it missing.

### One trial

The ten issues are fixed in advance, in owner-section format, under the git-ignored `docs/trials/local/`. Each one is filed with its body exactly as written. Every trial starts from the same ETS commit and no trial PR is merged, so the trials don't depend on each other. Each trial went the same way:

1. The `agent` label reaches the server through the tunnel. The feature request and plan comment appeared within 3 seconds.
2. The plan comment is read against the issue, and the owner comments `/approve`.
3. The attempt runs.
4. The gates run, `Land`, `Finalize` and `OpenPullRequest` produce a draft PR, `ReadChecks` waits for the `test` check, and `MarkReady` flips the PR.

The trial log records, per trial: base SHA, attempts, gate results, PR, CI result, cost and duration. Cost and duration come from `internal/store/trial_report.sql`, the query `conclave report` runs.

### The drills

- **Cancel A (trial 5).** `/cancel` was posted 48 seconds into the attempt. The signal reached the workflow in 2 seconds, and the cancel request went to the child immediately. The attempt stopped 37 seconds later, on its next heartbeat. Cancellation reaches an activity only through a heartbeat, and `RunAttempt` heartbeats every 30 seconds (`internal/activity/heartbeat.go`). The sandbox was released, no PR was opened, and the status comment read `cancelled`.
- **Worker kill (trial 6).** The worker was `kill -9`ed 45 seconds into its attempt. Temporal timed the activity out at exactly 2 minutes after its last heartbeat. When the second worker started, it ran `SettleAttempt`, which recorded ordinal 1 as `errored` with reason `sandbox_error`, the reason every attempt-activity error is settled with (`taskRun.settle`). It then ran `ReleaseAttemptSandbox` on the dead worker's container and started ordinal 2. Nothing that had completed ran again. The attempt itself is not resumed, because it runs with `MaximumAttempts: 1`. The workflow is resumed.
- **Halt (trial 8).** `conclave halt` was run with the attempt in flight. It recorded the halt, opened the halt alert issue and paused the reaper. It then cancelled the run, whose own disconnected cleanup released the sandbox 44 seconds later. Its sweep found nothing left and posted the "halt complete" comment. The worker exited 3 minutes 5 seconds after the halt: the `-halt-grace` period plus one 5-second poll. `conclave resume` cancelled no runs, because none had started during the halt, and unpaused the reaper. The run stays cancelled.

### The two fixes

- **The implementer is told its constraints.** Trial 6's second attempt failed `diff_size` at 528 changed lines against a cap of 400. The plan's `Constraints` reach `agent.Runner` as `SessionInput.Limits`, and `docs/agent-roles.md` lists them as implementer input, but `taskMessage` never wrote them. `writeConstraints` (`internal/agent/prompt.go`) now adds a Constraints section after the repository profile and above the untrusted issue text. It gives the changed-line cap and how lines are counted, the protected paths (changing one fails), the execution-surface paths (flagged), and the dependency policy. `Implementer.PromptVersion` moved to `implementer-v2` (decision 0006), so attempts from before the fix stay apart in provenance. The retry on `implementer-v2` delivered at 259 changed lines.
- **Nested bullets stay in their criterion.** Trial 9's plan had ten criteria for seven. One criterion was a lead-in with three sub-bullets, and `listItems` (`internal/activity/plan.go`) took any list marker at any indent as a new item. Now the first item's indent is the top level. A deeper item continues the item before it, as a plain continuation line already did, with a tab counting four columns. Trial 10's plan, with five sub-bullets under one criterion, parsed to its seven.

Each fix needed a worker restart, done between trials. Trial 6 was resumed with `/retry`, which continues the same trial rather than starting a new one.

### The result

| | |
|---|---|
| Delivered | 7 of 10 (trials 1, 2, 3, 4, 6, 7, 9) |
| Cancelled | 2 (trial 5 by `/cancel`, trial 8 by the halt) |
| Unresolved | 1 (trial 10, `needs_attention` after two `wall_clock` stops) |
| Spend | $51.41 over the ten |
| Cost per resolved task | $7.34 (all spend over the seven delivered) |
| Time per task | 488 s mean approve-to-delivered, 286 s median |
| Orphans | none: `UnreleasedAttempts` empty, no `conclave.sandbox` container |

Trial 10 ran out of time on both attempts. Every tool call took under two seconds, but model turns produced up to 23k output tokens and took one to three minutes each. Both attempts also spent turns on the CRLF line endings of the files they edited, then converted them. Neither is a platform fault, and both are recorded in decision 0016.

## Where to look

| File | What's in it |
|---|---|
| `docs/trials/local/log.md` (git-ignored) | The trial log: pre-flight, one row per trial, the drills, the after-trial checks, the report summary |
| `docs/trials/local/NN-*.md` (git-ignored) | Each issue as filed, with issue number, label time, base SHA and final state |
| `docs/trials/README.md`, `webhook-tunnel.md` | What a trial is and what it records; starting the tunnel |
| `internal/agent/prompt.go` | `taskMessage`, `writeConstraints` |
| `internal/agent/role.go` | `Implementer`, `PromptVersion: "implementer-v2"` |
| `internal/activity/plan.go` | `ParseIssue`, `listItems`, `indentWidth` |
| `internal/store/trial_report.sql` | The report query the figures above come from |
| `docs/decisions/0016-milestone-1-deferrals.md` | "Found in the ticket 10b trials" |

## What the tests prove

- **The agent is told the limits it is gated on, by the platform.** agent: `TestTaskMessageStatesConstraints` runs a session through the fake provider. It checks that the first request states the cap, the protected and execution-surface paths and the dependency policy, each above the untrusted issue label. A second case sends issue text claiming a 5000-line cap. The platform's 400 is the only cap stated, and the claim stays under the label. The test fails if `writeConstraints` is not called.
- **Nested bullets don't split a criterion.** activity: `TestParseIssueNestedItems` covers a lead-in with two sub-bullets, and a tab-indented item under an indented first item. Both cases fail on the old `listItems`. `TestParseIssue` still passes unchanged.
- **The live behaviours** have no new automated test. Their evidence is the trial log, and each rests on a 10a or earlier test:
  - the worker kill on `TestWorkerKilledMidAttemptResumesOnSecond`;
  - the halt on `TestHaltAgainstLiveTemporal` and `TestWatcherStopsAfterGrace`;
  - cancellation cleanup on `TestCancelDuringAgentPhase`.

## Left open

- **Trial 10 is unresolved.** After one `/retry` (the owner's choice) it was left in `needs_attention`. Its workflow is still open, holding no sandbox. `/cancel` ends it, and a new label starts a new run.
- **The implementer's 20-minute wall clock** was not raised: it is a starting value to revise with Milestone 2's latency data (decision 0016).
- **CRLF files** cost the agent turns, and nothing tells it a file's line endings (decision 0016).
- **Cancellation waits for the next heartbeat**, so an attempt stops up to about 40 seconds after `/cancel` or a halt.
- **Alert-log times** from Postgres carry the local offset, others UTC. This is cosmetic.
- **Trial PRs are unmerged.** They are drafts flipped to ready on ETS. Some touch the same files, so the owner resolves conflicts when merging them. Branch cleanup is FR-S20, deferred to Milestone 4.
- **Decisions 0002 and 0003** are still owed by the owner, so NFR-M9 is not met until they exist.

## Check your understanding

1. A worker is killed 45 seconds into an attempt. When does Temporal notice, what does the next worker record for that attempt and why that reason, and why doesn't it resume the attempt itself?
2. `/cancel` is posted while an attempt runs. Why does the attempt take up to a heartbeat interval to stop, and which code releases its sandbox?
3. During the halt drill the halt's own sweep released no sandboxes, yet none was left over. Why?
4. Why does the constraints section sit above the issue text, and what stops an issue from raising its own cap?
5. Why did the constraints fix bump the prompt version when the system prompt was unchanged?
6. A trial is resumed with `/retry` after a fix. Why does it still count as one trial, and what would make it two?
