# Ticket 8b — Commands and status surface

**Status:** in review on branch `ticket/8b` · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-8b--commands-and-status-surface), [`Ticket 8 Split — Conclave.md`](../../tickets/Ticket%208%20Split%20—%20Conclave.md#ticket-8b--commands-and-status-surface) · **Requirements:** FR-S11, FR-S12, FR-SEC10 · **Review list:** RL-9, RL-13 · **Decision:** [0013](../../decisions/0013-commands-and-status-surface.md)

## In one paragraph

The owner can now steer a run from the issue. `/retry` from `needs_attention` re-checks the budget caps and continues in the phase the feature request left. It keeps every attempt and its cost, and a run that stopped during planning still waits for a fresh `/approve`. A command that doesn't fit the current status gets a reply and changes nothing. `/status` and `/help` answer, and `/revise` and `/ask` say they are not available in Milestone 1. Every owner command gets one row in a new append-only `commands` table, accepted or rejected, with its actor and the status it was decided in. One status comment on the issue is edited in place after every move. It names the status, the reason, any cap that was hit, the phase left, and the commands available. A command for an issue whose workflow has ended (`delivered`, `cancelled`, or never started) is answered by a short `ClosedCommandWorkflow` on the worker.

## Why it exists

Ticket 8 routed every problem to `needs_attention` but left the owner with only `/cancel` as a way out, and dropped every other command silently. FR-S11 asks for `/status`, `/retry`, `/cancel` and `/help`. FR-S12 asks for a comment that explains what happened and the options left. FR-SEC10 asks for every command to be recorded with its actor and time. RL-9 is the lifecycle as the owner sees it: which commands are valid in each status, and what an invalid one does. RL-13 is why `/retry` is a compare-and-set move like every other.

Three constraints shaped it:

- **Deployment.** A ticket-8 run may be in flight when this code deploys. So the change is behind `workflow.GetVersion`, as decision 0012 promised.
- **No keys on the server.** The receiver holds no database access and no App key. A command to a closed run therefore cannot be answered on the server; it goes to a workflow on the worker.
- **Responses don't steer the run.** A reply or a status comment that fails must never send the run to `needs_attention`, which would loop. So responses are sent apart from the run's own steps.

## How it works

**The version gate.** `FeatureWorkflow` reads `GetVersion("commands-8b", DefaultVersion, 1)` first, into `featureRun.v8b`.

- With `v8b` false, the run behaves exactly as ticket 8: `onCommandV0` acts on `/approve` and `/cancel` only. It sends no replies, writes no audit rows and posts no status comment.
- The child task workflow keeps the ID `<wf>/task/<taskID>` for its first run. A run resumed by `/retry` adds `/<ordinal>` (`featureRun.childID`).

**One command table.** `validCommands(status)` in `internal/workflow/signals.go` is the `ARCHITECTURE.md` table plus `/help` in every live status, in the table's order. `commandApplies` is membership in it. The gate, the `/help` reply and the status comment's command list all read this one function.

**Deciding a command.** `onCommand` runs in the signal receiver's coroutine, against `f.st.Status` at that moment:

1. A command from anyone but the owner, or a comment ID already seen (`recentIDs`), is logged and dropped, with no row. The receiver already refuses both.
2. Not in the table: reply `not_valid`, row `rejected not_valid_in_status`.
3. `/revise` or `/ask` where the table lists them: reply `not_available`, row `rejected not_available`.
4. Otherwise `accept`:
   - `/approve` and `/cancel` set their latch, with row `accepted`. A second one while the first is pending is `rejected already_pending`.
   - `/status` and `/help` reply, with row `accepted`.
   - `/retry` is queued in `f.retries` and audited later, once its cap check has decided it.

Decisions don't call activities. They append a `commandEvent` (the command's `activity.CommandRef`, a snapshot `RunView`, outcome, reason, reply kind) to `f.outbox`.

**The responder.** `startResponder` runs one `workflow.Go` coroutine on a disconnected context. It sends outbox items in order:

- for a command: `RecordCommand`, then `ReplyToCommand` if a reply is due;
- for a status refresh: `UpsertStatusComment`. A refresh that a later one in the outbox supersedes is skipped.

These use `responseOptions`, with a 10-minute schedule-to-close. Failures are logged and never route the run. Every return of `FeatureWorkflow` calls `drain`, which records any `/retry` still queued as `rejected cancelled`, then waits for the responder to finish. An accepted `/cancel`, or a `/help` just before Temporal cancels the workflow, therefore has its row before the workflow closes.

**The status comment.** `transitionAs` records `Reason` and `PhaseLeft` when the move is to `needs_attention`, clears them on any other move, and queues `refreshStatus(f.view())`. So the comment follows every move. `UpsertStatusComment` (`internal/activity/comment.go`) finds the App's own comment carrying `conclave-status:<feature request ID>` and edits it, or creates it. The body comes from `statusComment` in `internal/activity/status.go`:

- a header line;
- the status, and in `needs_attention` the reason in words (`describeReason`, naming the cap and its config key) and the phase left;
- the cap figures after a refused `/retry`;
- the commands available, each in backticks;
- the marker.

**`/retry`.** Ticket 8's `awaitCancel` became `awaitOwner`. It waits for `/cancel` (which wins) or a queued `/retry`, and calls `retry` (`internal/workflow/retry.go`):

1. **`CheckCaps`** reads `CostLedger.Spend` (settled plus reserved) against `Deps.RouterConfig.Budgets`. Spend at or over a cap counts as exceeded, and the feature request cap is named before the monthly one. While over, `overCap` queues a reply naming the cap, a row `rejected over_cap:<kind>`, and a status refresh carrying the figures. The run stays put, and `awaitOwner` waits again.
   - If the caps could not be read, the reply is `resume_failed`.
   - If a `/cancel` arrived meanwhile, the `/retry` is `rejected cancelled`.
2. **`resume`** compare-and-sets `needs_attention → PhaseLeft` as `github:<actor>`, queues the row `accepted` (status `needs_attention`), and refuses any other `/retry` still queued (`not_valid`).
3. **It returns the steps to run.**
   - From planning, the whole `planSteps` list again, with `f.approval` cleared. `Analyze` and `PostPlanComment` are idempotent, so they return the existing plan and comment, and the run waits for a new `/approve` (FR-S5).
   - From executing, `resumeExecute`:
     - a passed task (`f.lastTask`, kept by `routeTask`) is delivered again;
     - a task never created is created;
     - otherwise **`ResumeTask`** moves the task `needs_attention → running` and returns one past its highest ordinal. The child runs from there, and earlier attempts and their `cost_events` are untouched.

`run(ctx, steps)` is the same loop ticket 8 had, now re-enterable: each step does nothing when its status doesn't apply.

**Terminal statuses.** `Dispatcher.dispatch` (`internal/github/dispatch.go`) handles a signal that returns `ErrNoWorkflow` by calling the new `WorkflowStarter.StartClosedCommand`. `cmd/server`'s `temporalStarter` starts `ClosedCommandWorkflow` under `ClosedCommandWorkflowID` (`command/<repoID>/<issue>/<commentID>`) with REJECT_DUPLICATE. So a redelivery is logged `duplicate` and answered once; the first is logged `closed_run`. The workflow (`internal/workflow/closed.go`):

1. drops a non-owner command;
2. runs `LookupFeature`, which reads the feature request by `feature_requests.workflow_id` and whether the comment is already in `commands`. An already-audited comment was decided by its live run and is left alone;
3. otherwise records the command `rejected` and replies, with `closedDecision`:
   - `closed_run` for delivered or cancelled;
   - `not_available` for `/revise` on delivered;
   - `no_run` for no feature request;
   - `no_workflow` for a live status with nothing running.

**The audit table.** Migration `00008_commands.sql` creates `commands`, keyed `UNIQUE (comment_id)`. It has CHECKs on the command word, `actor LIKE 'github:%'`, the status, the outcome, and "reason exactly when rejected". `conclave_app` gets SELECT and INSERT only. `store.RecordCommand` inserts with `ON CONFLICT (comment_id) DO NOTHING`.

**Idempotency, per new activity** (RL-2):

| Activity | If it dies after its effect | Answer |
|---|---|---|
| `RecordCommand` | A retry meets the unique comment ID and inserts nothing. | Idempotent retry |
| `ReplyToCommand` | A retry finds the App's reply carrying `conclave-reply:<comment ID>` and posts nothing. | Idempotent retry, GitHub as record |
| `UpsertStatusComment` | A retry finds the comment by its marker and edits it to the same body. | Idempotent retry, GitHub as record |
| `CheckCaps` | It only reads. | Idempotent retry |
| `ResumeTask` | A retry finds the task already `running`, moves nothing, and computes the same ordinal. No attempt is created there. | Idempotent retry |
| `LookupFeature` | It only reads. | Idempotent retry |

## Where to look

| File | What's in it |
|---|---|
| `internal/workflow/signals.go` | `validCommands`, `commandApplies`, `notAvailableInM1`, `onCommand`/`onCommandV0`, the outbox, `startResponder`, `send`, `drain` |
| `internal/workflow/retry.go` | `retry`, `overCap`, `refuseRetry`, `resume` |
| `internal/workflow/feature.go` | `versionCommands`, the step loop, `awaitOwner`, `resumeExecute`, `childID`, `transitionAs`'s status refresh |
| `internal/workflow/closed.go` | `ClosedCommandWorkflow`, `closedDecision` |
| `internal/workflow/options.go` | `responseOptions` |
| `internal/activity/command.go` | `RecordCommand`, `LookupFeature` |
| `internal/activity/caps.go` | `CheckCaps` |
| `internal/activity/comment.go` | `findMarked`, `UpsertStatusComment`, `ReplyToCommand` |
| `internal/activity/status.go` | The words: `statusComment`, `replyBody`, `describeReason`, `codeSpan` |
| `internal/activity/task.go` | `ResumeTask` |
| `internal/activity/types.go` | `CommandRef`, rejection reasons, `RunView`, `CapCheck` and the inputs |
| `internal/store/commands.go`, `migrations/00008_commands.sql` | The audit table and `RecordCommand`/`CommandRecorded` |
| `internal/github/dispatch.go` | `StartClosedCommand` on `WorkflowStarter`, `ClosedCommandWorkflowID`, the `closed_run` path |
| `cmd/server/temporal.go`, `cmd/worker/main.go` | Starting and registering `ClosedCommandWorkflow` |

## What the tests prove

**The command table and per-status rejections**
- All seven commands in all six statuses match the table written out in the test (workflow: `TestCommandApplies`, `TestValidCommandsOrder`).
- In each live status, every command the table doesn't list gets a reply and a rejected row, and the run moves exactly as without it (`TestInvalidCommandPerLiveStatus`, four sub-tests).
- `/revise` and `/ask` are answered "not available" (`TestRevAskNotAvailable`). `/status` reports the status (`TestStatusReply`). `/help` lists exactly the valid commands in each live status (`TestHelpListsValidCommands`).
- Terminal statuses: `TestClosedCommandWorkflow` covers delivered, `/revise` on delivered, cancelled, no run, and live-without-workflow. `TestClosedCommandWorkflowIgnores` covers a stranger and an already-audited comment.
- The dispatcher hands a no-workflow command to the closed-run workflow under the comment's ID, and a redelivery is a duplicate (github: `TestNoWorkflowStartsClosedCommand`). The starter uses RejectDuplicate (`cmd/server`: `TestStartClosedCommand`).

**`/retry`**
- After a raised cap, the run continues in executing at ordinal 2, and attempt 1 is intact (`TestRetryAfterCapRaised`).
- Still over the cap, the run stays in `needs_attention`; the reply and the comment name the cap, and the row is `over_cap:feature_request` (`TestRetryStillOverCap`).
- After a planning stop, the run waits for `/approve` with no task (`TestRetryFromPlanningWaitsForApprove`). An `/approve` accepted before an approval that failed doesn't carry over (`TestRetryAfterFailedApproveNeedsNewApprove`).
- After a delivery stop, delivery is retried without a new attempt (`TestRetryAfterDeliverStop`). A second simultaneous `/retry` is refused, not saved (`TestSecondRetryRefusedAfterResume`).
- In the database, `ResumeTask` twice makes one transition, returns the same ordinal, and leaves the cost intact (activity: `TestResumeTaskTwice`). It refuses a running attempt or a cancelled task (`TestResumeTaskRefuses`). `CheckCaps` treats under as passing and at or over as exceeded, for each cap, in order (`TestCheckCaps`).

**The status comment**
- One comment, posted once and edited after (workflow: `TestStatusCommentEditedInPlace`; activity: `TestUpsertStatusCommentTwice`, including a lost reply on create).
- It names the reason, the cap and the phase left (`TestStatusCommentNamesReason`, `TestStatusCommentNamesCap`).
- No platform comment parses as a command, even when an error detail carries `\n/cancel` and backticks (`TestPlatformCommentsNeverReadAsCommands`, NFR-U2a).

**The audit**
- One row per owner comment, none for a redelivery or a stranger (`TestEveryCommandAudited`). The row is written before the workflow closes, by `/cancel` and by Temporal cancellation (`TestCancelAuditedBeforeClose`).
- In the database, the row is recorded once with the first decision standing (`TestRecordCommandTwice`, `TestRecordCommandBeforeFeatureRequest`). Bad rows are refused permanently (`TestRecordCommandRefusesBadRecord`). `LookupFeature` returns the feature request and reports an already-audited comment (`TestLookupFeature`). `ReplyToCommand` posts one reply (`TestReplyToCommandTwice`).
- In the store, `conclave_app` cannot UPDATE, DELETE or TRUNCATE `commands` (`TestAppRoleCannotAlterAppendOnlyRows`). The CHECKs refuse what the audit must not hold (`TestCommandsSchemaChecks`). `RecordCommand` absorbs a repeat (`TestRecordCommandOncePerComment`).

**Replay**
- `TestReplayMidAttempt` and `TestReplayDetectsRerun` pass on the unchanged ticket-8 fixtures. That proves the version gate keeps an in-flight run deterministic.

## Left open

- **No v8b replay fixture.** No v8b history is captured; the existing fixtures cover the old path only. A capture mid-`/retry` with `CONCLAVE_CAPTURE_REPLAY=1` would need a second fixture file.
- **The cap digest goes stale.** A raised cap needs a worker restart. The plan body's `router.yaml` digest still names the file as it was at planning (decision 0013).
- **`/revise` and `/ask`.** They wait for the planner (Milestone 4).
- **Narrow audit gaps.** A command that lands in the instant between `drain` and the workflow's completion is not audited. The receiver's log keeps it.
- **A busy outbox.** A status refresh waits behind replies in the outbox. During a GitHub outage each response gives up after 10 minutes.
- **Owed by ticket 9.** Its migration is now 00009 and its decision record 0014.

## Check your understanding

1. A `/retry` arrives while the feature request is still over its monthly cap. Name every row, comment and transition that results.
2. Why does a run that stopped during planning wait for a fresh `/approve` after `/retry`, even if `/approve` had been accepted before the stop?
3. A `/cancel` arrives for an issue whose run is already `cancelled`. Trace it from the dispatcher to the audit row. What happens when GitHub redelivers it?
4. Why is the audit row for `/retry` written after the cap check, while `/approve`'s is written at receipt?
5. What would go wrong on replay if `GetVersion` were removed and a ticket-8 run's history were replayed by this code?
6. Why is the responder on a disconnected context, and what would an accepted `/cancel` lose if it were not?
