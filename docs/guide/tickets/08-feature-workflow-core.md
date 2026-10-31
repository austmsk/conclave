# Ticket 8 — Feature workflow core

**Status:** in review (`ticket/8-b`) · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-8--feature-workflow-core), [`ticket-8-pre-plan.md`](../../tickets/ticket-8-pre-plan.md#carried-into-ticket-8) · **Requirements:** NFR-R1, NFR-R2, NFR-R3, FR-S5, FR-S12, FR-S19 (pinning) · **Review list:** RL-1, RL-2, RL-3, RL-13, RL-14, RL-16 · **Decision:** [0012](../../decisions/0012-temporal-workflow-shape.md)

## In one paragraph

A labelled issue now starts a Temporal workflow. `FeatureWorkflow` parses the owner's issue into a plan with no model call, posts the plan as a comment, and waits as long as it takes for `/approve`. Approval pins the default branch's head as the base commit. The workflow then runs the task as a child, `TaskWorkflow`, which runs one attempt as one activity: sandbox, agent, freeze, gate, record, release. Whatever happens (a gate failure, a runner limit, a platform cap, a crash, a permanent error) the feature request lands in `needs_attention` with a reason, never an automatic failure, and waits there for `/cancel`. Sandboxes are released from a disconnected context on every exit, so cancellation cannot leak one. Delivery is a stub until ticket 9: it verifies the artifact and stops in `needs_attention` with "delivery arrives in ticket 9".

## Why it exists

Everything before this ticket was a component. This is the part that has to survive restarts, retries and cancellation (NFR-R1), so it is where plausible-looking code is most often wrong:

- A retried activity must not post twice, pin twice or run an agent twice (RL-2, NFR-R2).
- Cleanup started on a cancelled context never runs (RL-3, NFR-R3).
- A state change must be a compare-and-set on what the workflow last saw (RL-13).
- History must hold IDs and hashes, never transcripts or diffs (RL-14).
- A timeout or a retry policy computed from live configuration can change under a run in flight (RL-16).
- `/approve` must never be blind and must pin exactly one base commit (FR-S5, FR-S19), and every problem routes to `needs_attention` (FR-S12).

## How it works

**Start.** `cmd/server`'s `temporalStarter` starts `FeatureWorkflow` under `github.FeatureWorkflowID(repo, issue)` with `RejectDuplicate`, so a redelivered label is `ErrAlreadyStarted`. Commands are signalled on their own name; a missing workflow is `ErrNoWorkflow`.

**Analyze.** `activity.Analyze` creates the feature request and, only when the issue's author is the owner (`github.Classify`), parses its `Task`, `Acceptance criteria` and `Hints` sections (`ParseIssue`) into plan version 1. The plan body (`PlanBody`) is the frozen agent input: task spec, quarantined issue text, repo profile, constraints from `gates.yaml`, the repository (full name and numeric ID), and the SHA-256 of every deploy file. A missing section or a stranger's issue returns a `Problem`, and the workflow stops in `needs_attention` with `error:<problem>`.

**Plan comment and approval.** The workflow moves to `awaiting_approval` and `PostPlanComment` posts the plan with a visible marker (`conclave-plan:<plan id>`). Signal handlers (`signals.go`) run in the workflow's scheduler. Each drops a command from anyone but the owner, a comment ID it has seen (a bounded set of 64), and a command that does not apply in the current status (`commandApplies`): `/approve` only in `awaiting_approval`, `/cancel` in any non-terminal status. `Approve` clones the default branch, reads its head, and approves the plan and moves the feature request to `executing` in one transaction.

**Task.** `CreateTask` records task `T1` from the plan body. `FeatureWorkflow` starts `TaskWorkflow` (ID `<feature id>/task/<task id>`, `ParentClosePolicy` terminate, `WaitForCancellation`) and waits for it or for `/cancel`, which cancels the child and then waits, on a disconnected context, for the child to finish its cleanup.

**Attempt.** `TaskWorkflow` takes only serializable input: task ID, ordinal, base commit, the repository's full name and the role. It calls `PrepareAttempt` (creates the attempt row with the provenance of the role's spec from `Deps.Roles`, and computes its timeout), defers `ReleaseAttemptSandbox` on a disconnected context, and runs `RunAttempt` on the paired attempt queue with `MaximumAttempts: 1`. Inside, `RunAttempt` heartbeats from its own goroutine, has the `TreeSource` prepare the plan's repository at the base commit (`GitHubTrees` clones through the App; Milestone 2 substitutes its own, so a task needs no GitHub), creates the sandbox, records its ID with `SetAttemptSandbox` as the first write after `Create` (releasing the sandbox at once if that write fails), runs `agent.Runner`, freezes and records the artifact hash, gates it, and records the outcome with `FinishAttempt` on a context that survives cancellation. If the activity returns an error, `SettleAttempt` records the attempt `errored` if it is still running and returns what Postgres holds, and the task routes on that record.

**Routing.** `taskRun.route` applies the outcome table: passed → task `done`; failed → task `needs_attention`, feature request `attempt_failed`; `budget_cap` → `budget_cap:<kind>` read from the error type; errored → a new ordinal once, then `error`; a permanent error → `error` with no new ordinal. A passed task goes to `Deliver`, the stub. `featureRun.needsAttention` steps `awaiting_approval` back to `planning` first, because that move is not legal and `/retry` must not skip `/approve`. The phase left is kept in `Status.PhaseLeft` for 8b.

**Cancellation.** `/cancel`, or Temporal cancelling the workflow, closes the task (`CancelTask`: running attempts errored/cancelled, recorded sandboxes released, task cancelled) and moves the feature request to `cancelled`. When the workflow itself is cancelled this runs on a disconnected context. As the workflow ends, `ReleaseFeatureSandboxes` sweeps anything still recorded and unreleased.

**Worker.** `cmd/worker` loads every deploy file through `deployfile.Load` (recording digests), checks that `repos.yaml`, `toolchains.yaml`, `sandboxes.yaml` and `gates.yaml` name the same repositories (`sameRepositories`), reads its secrets from the environment and unsets them, seeds `repos`, fills the role map (`roles()`: the implementer in Milestone 1), refuses a `-sweep-after` that does not exceed the longest attempt timeout, runs `SweepOrphans` (the reaper half of NFR-R3), and runs two workers: the feature queue, and the attempt queue with one slot.

### What recovers each crash gap

| Activity | Outside action | If the worker dies after it, before the Postgres write |
|---|---|---|
| `Analyze` | none | Idempotent retry: creates keyed on (repo, issue) and (feature request, version) return the first rows |
| `PostPlanComment` | GitHub comment | Idempotent retry: the retry lists the App's comments and finds the marker; there is no Postgres write after the post |
| `Transition` | none | Idempotent retry: compare-and-set; `ErrAlreadyApplied` is success |
| `Approve` | read-only clone | Idempotent retry: a plan already approved returns its pinned SHA without cloning, so a moved branch is not re-pinned |
| `CreateTask`, `PrepareAttempt` | none | Idempotent retry: keyed creates |
| `RunAttempt` | Docker sandbox | Write-ahead intent: the sandbox ID is recorded right after `Create`; a death before that is covered by `Reap` by age in `SweepOrphans`; after it, by the deferred `ReleaseAttemptSandbox` and the sweep. A death after `FinishAttempt` is read back by `SettleAttempt`. Never retried by Temporal |
| `SettleAttempt` | none | Idempotent retry: finishes only from `running` |
| `ReleaseAttemptSandbox`, `ReleaseFeatureSandboxes`, `CancelTask` | Docker remove | Idempotent retry: `Release` of a removed sandbox is nil, then the column is written; the mark is never written before the remove |
| `Deliver` (stub) | none | Read only |
| `SweepOrphans` (startup) | Docker remove | Rerun at the next startup: rows stay unreleased until the remove succeeds |

## Where to look

| File | What's in it |
|---|---|
| `internal/workflow/feature.go` | `FeatureWorkflow`: analyze, approval wait, child, routing, cancellation, final sweep |
| `internal/workflow/task.go` | `TaskWorkflow`: ordinals, settle-from-Postgres, outcome routing, deferred release |
| `internal/workflow/signals.go` | Command receivers, `commandApplies`, the bounded seen-ID set |
| `internal/workflow/options.go` | Activity options, the attempt queue, `cleanupContext` |
| `internal/activity/attempt.go`, `attempt_run.go` | `PrepareAttempt` and `RunAttempt` |
| `internal/activity/activities.go`, `trees.go` | `Deps` (with the role map), `TreeSource` and `GitHubTrees` |
| `internal/activity/errors.go` | `Temporal`: the error mapping for the activity boundary |
| `internal/activity/plan.go`, `analyze.go`, `comment.go` | Issue parsing, the plan body, the plan comment |
| `internal/activity/release.go` | Release activities and `SweepOrphans` |
| `internal/activity/pgread.go` | Read-only queries by ID the store does not yet offer |
| `cmd/server/temporal.go` | The Temporal `WorkflowStarter` |
| `cmd/worker/main.go`, `config.go` | Worker wiring, deploy-file loading, the same-repositories check |
| `internal/workflow/testdata/*.json` | Histories captured mid-attempt for the replay test |

## What the tests prove

- **Every outcome-table row.** `TestOutcomeTable` (workflow), one case per row, plus `TestOutcomeTaskStatusBeforeCancel`, `TestCancelInAwaitingApproval`, `TestCancelDuringAgentPhase` and `TestNonRetryableErrorOutsideAttempt`.
- **Cancellation releases the sandbox.** `TestCancelDuringAgentPhase` (`/cancel`), `TestCancelParentReleasesChildSandbox` (Temporal cancels the parent; the child releases), `TestCancelTaskWorkflowDirectly`, `TestCancelInNeedsAttentionReleasesSandbox`. Replacing `NewDisconnectedContext` with the workflow context fails the first three.
- **Caps route to `budget_cap:<kind>` and keep the attempt.** The two cap rows of `TestOutcomeTable`; `TestTemporal` and `TestBudgetKindOf` (activity) for the type string.
- **No problem fails the workflow.** `TestNonRetryableErrorOutsideAttempt`, `TestPermanentAttemptErrorStartsNoOrdinal`, `TestAnalyzeProblem`.
- **Permanent errors are not retried; rate limits wait.** `TestPermanentRouterErrorNotRetried`, `TestRateLimitRetriedAfterRetryAfter`, `TestRetryableErrorIsRetried`, `TestAttemptNotRetried`, and `TestTemporal` for the mapping.
- **Long approval wait.** `TestApproveAfterLongWait` skips 30 days.
- **Signals.** `TestCommandApplies` (every command in every status), `TestSignals` (early, redelivered, stranger's, duplicated and 8b commands), `TestApproveDuringPlanningIsDropped` (an `/approve` sent while `Analyze` runs is dropped, not saved), `TestRecentIDs`.
- **Settle from Postgres.** `TestAttemptResultLostAfterRecording`, `TestAttemptWorkerDied`.
- **Replay.** `TestReplayMidAttempt` replays the captured histories offline and checks they are mid-attempt; `TestReplayDetectsRerun` shows the replayer refuses a workflow that runs `Analyze` twice, so the replay test can fail.
- **Role is data; the tree is a seam.** `TestRunAttemptRoleAndInstall` runs an attempt as `test_writer`, added only to the role map, and records its prompt version, with the install phase through `CreateWithInstall`. `TestPrepareAttemptRefusesUnconfiguredRole`, `TestNewRefusesMislabelledRole`. `TestRunAttemptTreeSourceNeedsNoGitHub` runs an attempt on a local tree with no GitHub clone; `TestGitHubTreesRefusesPlanWithoutRepositoryID`; `TestTaskWorkflowDirectly`.
- **One effect per activity, run twice.** `TestAnalyzeTwice`, `TestPostPlanCommentTwice` (including a lost reply), `TestTransitionTwice`, `TestApproveTwice` (the branch moves between runs), `TestCreateTaskTwice`, `TestPrepareAttemptTwice`, `TestSettleAttemptTwice`, `TestRunAttemptTwice`, `TestReleaseAttemptSandboxTwice` (including a death between Docker and Postgres), `TestReleaseFeatureSandboxesTwice`, `TestCancelTaskTwice`, `TestDeliverVerifiesPassedArtifact` (the stub's success path through `gate.Verify`), `TestDeliverRefusesUnpassed`, `TestDeliverRefusesUnverified`, `TestSweepOrphans`.
- **Write-ahead sandbox ID.** `TestRunAttemptSandboxIDNotRecorded`; `TestSweepOrphans` reaps an unrecorded sandbox; `TestSweepOrphansSparesLiveAttempts`; `TestLongestAttempt` for the sweep cutoff's floor.
- **History holds no tool output.** `TestCanaryNotInHistory` runs `TaskWorkflow` over the real activities with a model that reads a canary file, checks the canary reached the blob store, and checks no recorded payload or error carries it.
- **Traceable end to end.** `TestTraceableFromWorkflowID` walks `feature_requests.workflow_id` to plans, tasks, attempts, tool calls, cost events and state transitions.
- **Starter and worker.** `TestStartFeature`, `TestSignalCommand` (server); `TestStarterAgainstLiveTemporal` has a real server refuse a duplicate start, running and closed, when `CONCLAVE_TEMPORAL_ADDRESS` is set; `TestSameRepositories`, `TestLoadDeployRefusesShippedPlaceholders` (worker).

## Left open

- **The traceability join is walked over an ungated run.** `TestTraceableFromWorkflowID` stops before the gate, so `gate_results` is joined but empty; ticket 10's end-to-end run walks it over a gated attempt.
- **A Temporal cancellation during `Analyze`** leaves the feature request in `planning` if the activity committed but its result never reached the workflow: the workflow has no ID to close. Rare, and visible in the database.
- **The budget kind is lost** if `RunAttempt` records `budget_cap` and then dies before returning its error; the feature request's reason is then `budget_cap:` with no kind.
- **Read-only queries in `pgread.go`** belong in `internal/store`; ticket 8 was limited to one change outside its files.
- **No worker startup test over a valid configuration**: the shipped files are placeholders and `deploy/local/` is git-ignored.
- The replay fixtures were captured with the tests' fake activities; recapture after a deliberate workflow change.
- **The live starter test does not run in CI**, which has no Temporal server; run it with `CONCLAVE_TEMPORAL_ADDRESS=localhost:7233` against `make dev`.
- **Heartbeats stop at cancellation.** The heartbeat goroutine ends with the activity context, so the post-cancel record and release run without heartbeats under the 2-minute timeout. The workflow treats the attempt as cancelled either way.
- **`sandbox_error` covers failures before the sandbox exists**, such as a database error in `PrepareAttempt`, when the task stops without an attempt to blame.

## Check your understanding

1. Why does `RunAttempt` have `MaximumAttempts: 1`, and what records an attempt whose activity timed out?
2. What would go wrong if the deferred `ReleaseAttemptSandbox` used the workflow's own context?
3. Why does `needsAttention` step back from `awaiting_approval` to `planning` first?
4. A `/approve` arrives while the plan is still being parsed. What happens to it, and to its redelivery a minute later?
5. Which three mechanisms between them release a sandbox whose worker died mid-attempt, and which gap does each cover?
6. Why is the plan comment's marker visible text and not an HTML comment?
