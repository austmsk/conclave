# Ticket 10a — Kill switch, alerts and scheduled reaper

**Status:** in review · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-10a--kill-switch-alerts-and-scheduled-reaper), [`ticket-10-plan.md`](../../tickets/ticket-10-plan.md) · **Requirements:** FR-SEC11, FR-SEC31, NFR-R1, NFR-R3, NFR-M9 · **Review list:** RL-18 · **Decisions:** [0015](../../decisions/0015-kill-switch-alerts-and-reaper.md), [0016](../../decisions/0016-milestone-1-deferrals.md) · **Runbooks:** [`runbooks.md`](../../runbooks.md)

## In one paragraph

Conclave can now be stopped, can tell the owner when something needs them, and cleans up after itself on a timer.

- **The kill switch.** `conclave halt`, in a new `cmd/conclave` binary, records a halt in Postgres, raises a halt alert, cancels every open workflow, and sweeps every sandbox regardless of age. Workers see the halt, finish their cancellations for a grace period, then exit, and refuse to start until `conclave resume`.
- **Alerts.** Each alert is written to a local append-only log first, then opened as an issue in a private operations repository and assigned to the owner. A repeat comments on the open issue instead of opening another. Milestone 1 raises one on a halt, on a budget-cap stop, and on `conclave alert-test`.
- **The reaper.** The orphan sweep, which ran only at worker startup, now also runs every 15 minutes on a Temporal schedule.
- **Two smaller pieces.** A test kills a worker mid-attempt and shows a second worker carries the workflow on. One SQL query produces the trial report that ticket 10b and Milestone 2 read.

## Why it exists

FR-SEC11 asks for a single command that cancels all in-flight runs and stops workers, and the incident runbook's first step is "trigger the kill switch". Until now there was none. FR-SEC31 asks for alerts as issues, written locally first. NFR-R3 asks for a periodic reaper, and ticket 8 left the sweep running once at startup.

RL-18 shaped where each piece lives:

- The halt is in Postgres, not on GitHub, so it works when GitHub or the owner's account is the problem.
- The webhook server still holds no database access or App key.
- The alert path's token can open issues in one repository and nothing else, and that repository is never one an agent works in.

NFR-R1 moved here from ticket 8, whose testsuite runs no real workers. Exit-checklist item 3 asks for the trial-report query to exist before Milestone 2.

## How it works

### The halt

`store.SetHalt` inserts a `set` event into `halt_events` (migration `00010_halts.sql`). The application role may select and insert there, never update or delete. The insert runs under an advisory lock, and nothing is recorded if the latest event is already a `set`, so a second halt keeps the first one's ID and time. `store.CurrentHalt` reads the event with the highest ID.

`conclave halt` then:

1. raises `alert.Halt`;
2. pauses the `conclave-reaper` schedule;
3. lists running workflows and cancels each root one. A feature workflow cancels its own task child and releases its sandbox from a disconnected context. The CLI lists again until two listings in a row find none open (the listing trails a start by a moment) or `-drain` runs out;
4. calls `activity.SweepOrphans` with `olderThan` 0;
5. counts what is left;
6. raises `alert.HaltDone`, which shares the halt's key and so lands as a comment on the same issue. Each run of `conclave halt` is its own occurrence, named by its time, so a second halt during the same halt reports its own sweep.

The step-by-step is in [`runbooks.md`](../../runbooks.md#halt).

The worker side is `cmd/worker/halt.go`. At startup `refuseIfHalted` stops the worker while a halt stands. `watchHalt` polls every 5 seconds. When it sees a halt it waits `-halt-grace` (3 minutes, longer than the 2-minute attempt heartbeat timeout), then cancels the serve context with `errHalted`, and the worker exits. A failed read is logged and polled again.

`conclave resume` cancels every workflow started after the halt's time, which a label applied during the halt would have started, unless `-keep` is passed. It then calls `store.ClearHalt` and unpauses the schedule.

### An alert

`internal/alert` owns one alert's path. A constructor builds the `Alert` from typed identifiers:

- `Halt(haltID, reason, at)`;
- `HaltDone(haltID, result, at)`;
- `BudgetCap(where, kind, revision, at)`;
- `Test(at)`.

Each alert has a key (the condition, such as `budget_cap:<feature request>`) and an occurrence ID (this instance, such as `budget_cap:<feature request>:<revision>`). The text is fixed, and only the halt reason is free. That passes through `github.RedactText`, is joined into one line, and is cut at 200 characters.

`alert.Raise(ctx, log, issues, a)` does three things in order:

1. **Validate, then log.** It validates the alert (keys and `Where` values must look like identifiers) and appends an `alert` entry to the `Log`. If that write fails, nothing is sent.
2. **Deliver.** `deliver` lists open issues labelled `conclave-alert` through `github.App.ListOpenAlertIssues`, which keeps only issues the App's bot opened.
   - **An issue holds the key line.** If an issue's body has `conclave-alert-key: <key>` as a whole line, `update` comments on it. It skips the comment if the issue body, or one of the App's comments, already holds `conclave-alert-occurrence: <id>`.
   - **No issue holds it.** `CreateAlertIssue` opens one with labels `conclave-alert` and `severity:<s>`, assigned to `alerts.assignee`.
3. **Record the outcome.** It appends a `delivery` entry: `delivered`, or `failed` with the redacted error.

The markers are plain text because `sanitizeMarkdown` strips HTML comments from every outgoing body.

`Log` (`internal/alert/log.go`) opens its file with `O_APPEND|O_CREATE|O_NOFOLLOW`, mode 0600, refuses anything that is not a regular file, and writes each JSON line in one write followed by an fsync.

The GitHub side is `internal/github/alerts.go` and `config.go`. The `alerts:` block in `github.yaml` names the repository, its ID and the assignee. `RequireAlerts` makes it mandatory for the worker and the CLI. `permissionsFor` gives that repository's token `alertTokenPermissions` (`issues: write`, `metadata: read`), and `checkTokenReply` checks each reply against the set that was asked for. Every alert call goes through `alertsRepo`, so none can reach another repository.

### The pause alert

`featureRun.needsAttention` (`internal/workflow/feature.go`) calls `raiseAlert` after a successful move. `raiseAlert` (`internal/workflow/alert.go`) does nothing unless the run read version 1 of `alerts-10` and the reason passes `activity.IsBudgetCapReason`. It then runs the `RaiseAlert` activity with the feature request ID, the workflow ID, the reason and the new revision. The activity (`internal/activity/alert.go`) reads the repository and issue number from the database and raises `alert.BudgetCap`, so no alert text enters history. Its options give up after 10 minutes, and a failure is logged, so the `needs_attention` wait goes on.

### The reaper

At startup, `cmd/worker/reaper.go`'s `ensureReaperSchedule` creates the schedule `conclave-reaper`:

- every `-reap-every` (15 minutes, at least 1);
- overlap SKIP;
- action: `ReaperWorkflow` on the `conclave` queue, with `ReaperInput{OlderThan: -sweep-after}`.

If the schedule exists, startup updates its spec and action and keeps its paused state.

`ReaperWorkflow` (`internal/workflow/reaper.go`) runs the `SweepOrphans` activity with three tries. The activity refuses a cutoff at or below `Deps.LongestSandboxHold`, the longer of the longest attempt and `activity.FinalizeTimeout` (45 minutes). The worker's `checkSweepAfter` makes the same check at startup. The startup sweep still runs too.

### What each new activity does after a crash

| Activity | Outside action | If the worker dies after it, before the result is recorded |
|---|---|---|
| `RaiseAlert` | local log line, issue or comment | The retry writes the alert line again (readers group by occurrence) and finds the issue or comment by its occurrence line, so nothing is posted twice. |
| `SweepOrphans` | sandbox release, container removal | `Release` of a removed sandbox returns nil, `MarkSandboxReleased` is write-once, and `Reap` removes only what is still there. |

## Where to look

| File | What's in it |
|---|---|
| `cmd/conclave/` | `halt`, `resume`, `alert-test`, `report` |
| `cmd/worker/halt.go` | `refuseIfHalted`, `watchHalt`, `haltGrace` |
| `cmd/worker/reaper.go` | `ensureReaperSchedule`, `reaperSchedule` |
| `cmd/worker/main.go` | `-alert-log`, `-reap-every`, `-halt-grace`, `checkAlerts`, `checkSweepAfter` |
| `internal/alert/` | `Alert` and its constructors, `Log`, `Raise`, the `Issues` interface |
| `internal/github/alerts.go`, `config.go`, `appauth.go` | Alert-issue calls, `AlertsConfig`, `alertTokenPermissions`, `permissionsFor` |
| `internal/activity/alert.go` | `RaiseAlert`, `IsBudgetCapReason` |
| `internal/activity/release.go`, `activities.go` | `SweepOrphans` as an activity, `LongestSandboxHold`, `FinalizeTimeout` |
| `internal/workflow/alert.go`, `reaper.go` | The `alerts-10` gate, `raiseAlert`, `ReaperWorkflow`, `ReaperScheduleID` |
| `internal/store/halt.go`, `migrations/00010_halts.sql` | `halt_events`, `SetHalt`, `ClearHalt`, `CurrentHalt` |
| `internal/store/trial_report.sql` | The trial report query |
| `docs/runbooks.md` | Halt, budget cap, alert delivery failed, alert test |

## What the tests prove

- **The halt is append-only and repeatable.** store: `TestHaltStateLatestEventWins` walks set, clear and set as the application role, and `TestSetHaltTwiceStaysOne` keeps one row for two halts. `TestHaltNeedsReason` refuses a halt with no reason. `TestAppRoleCannotAlterAppendOnlyRows` attempts the update, delete and truncate of `halt_events`.
- **The kill switch cancels everything and cleans up.**
  - conclave: `TestHaltOrder` checks the steps run in order. `TestHaltTwice` shows a second halt continues the first. `TestHaltCancelsLateStarts` cancels a workflow that opens during the drain, and `TestHaltCancelsRootsOnly` leaves children to their parents.
  - conclave: `TestHaltDeliveryFailureStillHalts` shows a GitHub outage does not stop the halt, and `TestHaltReportsRemaining` covers the remaining counts. `TestResumeCancelsStartedDuringHalt` and `TestResumeKeep` cover resume.
  - `TestHaltAgainstLiveTemporal` runs it against a real Temporal server with real workers, and shows every workflow cancelled, every sandbox released and the workers exiting.
- **Workers obey the halt.** worker: `TestWorkerRefusesStartWhileHalted`, `TestWatcherStopsAfterGrace` (stops once, not before the grace period) and `TestWatcherKeepsPollingThroughReadErrors`.
- **An alert is logged before delivery, and delivered once.** alert:
  - Logging: `TestLogWrittenBeforeDelivery` shows the alert and the failure in the log when GitHub fails. `TestLogAppendsAndIsPrivate` shows reopening appends and the file is 0600. `TestLogRefusesSymlink` attempts the symlink.
  - Dedupe: `TestRaiseDedupesOpenIssue`, `TestRaiseTwiceOneComment`, `TestRaiseReopensAfterClose`, `TestKeyMatchesWholeLine` and `TestHaltDoneSharesKey`.
- **An alert cannot be steered or made to leak.**
  - alert: `TestSpoofedMarkerIgnored` plants an owner-written issue carrying the key line. `TestAlertCarriesNoSecretOrContent` puts a token and markup in the halt reason. `TestInvalidAlertRefused` refuses a `Where` that is not an identifier.
  - github: `TestListOpenAlertIssuesKeepsOwn` and `TestAlertErrorsCarryNoSecrets` cover the client side.
- **The alert token is the narrowest one.** github: `TestAlertTokenAsksOnlyIssues`, `TestAlertTokenWithContentsRefused` (a wider reply is discarded) and `TestRunTokenPermissionsUnchanged`. `TestAlertCallsNeedAlertsConfig` refuses every alert call without the block. `TestRequireAlerts` covers the requirement, and `TestCreateAlertIssue`, `TestCreateAlertIssueRefusals` and `TestListOpenAlertIssuesPaginates` cover the calls. worker: `TestCheckAlerts` refuses an alerts repository that is also a run repository.
- **A budget-cap stop raises one pause alert per stop, and nothing else does.**
  - workflow: `TestBudgetCapRaisesPauseAlert` has two stops and two alerts. `TestOtherStopsRaiseNoAlert` shows a failed attempt raises none. `TestRaiseAlertFailureStillWaits` shows the run still waits and `/cancel` still works.
  - activity: `TestRaiseAlertTwice`, `TestRaiseAlertSecondStopComments`, `TestRaiseAlertDeliveryFailureRetries` and `TestRaiseAlertRefuses`.
  - The existing replay tests (`TestReplayMidAttempt`, `TestReplayDetectsRerun`) replay every fixture unchanged under the new version gate.
- **The reaper is scheduled and cannot reap a live sandbox.**
  - workflow: `TestReaperWorkflowSweeps` and `TestReaperWorkflowRetriesThenFails`.
  - worker: `TestEnsureReaperScheduleIdempotent` updates in place and keeps the pause. `TestCheckSweepAfterCoversFinalize` shows a cutoff below Finalize's 45 minutes is refused.
  - activity: `TestSweepOrphansActivityRefusesShortCutoff` and `TestSweepOrphansActivityTwice`.
- **A worker killed mid-attempt does not lose the run (NFR-R1).** workflow: `TestWorkerKilledMidAttemptResumesOnSecond` runs two worker sets against a real Temporal server and stops the first mid-attempt. The second carries the workflow on: every activity completed before the kill ran once, the first attempt settles `errored` after its heartbeat timeout, and the second ordinal runs and completes. The attempt itself is not resumed: it runs with `MaximumAttempts: 1`.
- **The trial report adds up.** store: `TestTrialReport` seeds a delivered, a stopped and a cancelled feature request and checks every figure.

## Left open

- **Owner steps before the worker starts again:**
  - Create the private operations repository, add it to the App's installation, and set `alerts.repository_id` in `deploy/local/github.yaml` (`GET /repos/<owner>/<name>` → `.id`).
  - Until then the worker and the CLI refuse to start. So does the server: a partly filled `alerts:` block fails validation for every process that loads the file.
  - Pass `-alert-log` to the worker and the CLI, and optionally `chflags uappnd` the file.
- **GitHub Pro and the ETS ruleset.** A ruleset on ETS `main` with no bypass actors: require a PR and the `test` check, and block force pushes. Check it with `gh api repos/austmsk/ets/rules/branches/main`. Until it is in place, FR-SEC16's branch rules are unmet.
- **A halt removes in-use sandboxes.** An attempt running during the sweep ends `errored`, and its spend is kept.
- **A halt needs Postgres.** With the database down, `conclave halt` cannot record a halt. Stop the worker processes by hand.
- **Deferred alert sources:** auto-halt on repeated unknown-actor failures, the weekly digest batch, canary alerts and a second channel (decision 0016).
- **Ticket 9's open review items** are listed as known gaps in decision 0016.
- **Ticket 10b** runs the ten trials, kills a worker during a live one, exercises the halt with a run in flight, writes the trial log under git-ignored `docs/trials/local/`, and confirms zero orphans.
- **Decisions 0002 and 0003** (ticket 2) are still owed by the owner.

## Check your understanding

1. Why does the halt live in a Postgres table rather than in Temporal or on GitHub, and why can the application role not delete a row from it?
2. A label is applied while the system is halted. What starts, what runs it, and what does `conclave resume` do about it?
3. Why does a worker wait three minutes after seeing a halt instead of exiting at once?
4. The worker dies after `CreateAlertIssue` succeeded but before `Raise` returned. What does the retry find, and why does it post nothing new?
5. Why are the alert markers plain lines of text and not HTML comments, and why does delivery ignore an issue the owner opened with the same line?
6. What stops someone from setting the reaper schedule's cutoff to five minutes, and why must the cutoff exceed Finalize's bound as well as the longest attempt?
