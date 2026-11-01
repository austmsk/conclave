# Ticket 10 plan: 10a hardening code, then 10b live trials

## Context

Ticket 10 is the last Milestone 1 ticket: ten live trials against `austmsk/ets` (7/10 PRs passing CI, zero orphaned sandboxes, 2 cancellations), plus the hardening M1 still lacks. Today none of the hardening exists:
- the reaper runs once at worker startup (`internal/activity/release.go:88` `SweepOrphans`, called from `cmd/worker/main.go:112`);
- there is no kill switch, no alert path, no create-issue call, no Temporal Schedule, and no `cmd/conclave`;
- NFR-R1 has only replay proof, since ticket 8's testsuite has no workers.

Owner decisions (2026-10-31):
- Split into **10a** (code: branch `ticket/10a`) and **10b** (live trials: its own session).
- Kill switch is a **CLI**, `conclave halt` / `conclave resume`.
- Alerts go to a **new private ops repo** (e.g. `austmsk/conclave-ops`), not the public Conclave repo.
- Trial issues and the trial log live in **git-ignored `docs/trials/local/`**. Only the README/template and the report SQL are tracked.
- Alert sources: **halt** (kill switch) and **pause** (budget-cap stop), plus `conclave alert-test`. Unknown-actor auto-halt, the weekly digest and canary alerts are deferred, each recorded.
- During a halt the server keeps no DB access. **`resume` cancels every workflow started after the halt** unless `-keep` is passed.
- Claude drafts 0015/0016 as `proposed`. The owner writes 0002/0003 before M1 closes, and 0005 stays reserved.
- Webhook tunnel not done → **step 0**. Secrets file and `checks: read` are confirmed.

## Step 0 — owner setup (before 10b; can run alongside 10a)

1. `brew install cloudflared`, then run `cloudflared tunnel --url http://127.0.0.1:8080`. Set the App's Webhook URL to `https://<random>.trycloudflare.com/webhooks/github` (the address changes each restart; avoid smee.io). Verify by redelivering the `ping` from the App's Advanced tab with `cmd/server` running.
2. Create the private ops repo, add it to the App installation, and note its numeric ID.
3. Write `deploy/local/router.yaml` with real prices, output caps and FR-SEC9 caps. None exists today, and the shipped `deploy/router.yaml` has `-1`/`0` placeholders that fail startup.
4. **GitHub Pro and ETS branch protection (you, during 10a).** Earlier, the protection and rulesets APIs returned 403 "Upgrade to GitHub Pro" on private ETS, so FR-SEC16's "branch rules without App bypass" has been unmet since ticket 6. Once Pro is active:
   - Create a **ruleset** (not classic protection) on `main` with the bypass list empty, so neither the App nor you can bypass it. It should:
     - require a pull request;
     - require the status check `test` (the check named in `deploy/local/gates.yaml`'s `ci:` block);
     - block force pushes and deletion.
   - Use a ruleset because `GET /repos/austmsk/ets/rules/branches/main` reads it with plain repo read access. Classic protection needs `administration`, which the App must never hold (FR-SEC15).
   - Verify by running `gh api repos/austmsk/ets/rules/branches/main`. It should list `pull_request`, `required_status_checks` and `non_fast_forward`, and the ruleset's `bypass_actors` should be empty.
   - Record the result in the 10a PR description, and in the checklist's FR-SEC16 row as owner-config evidence beside ticket 6's `TestPushOnlyToAgentBranches`.
   - Ticket 9's flow still works under these rules. It pushes only `agent/` branches, opens draft PRs and never merges, and CI already runs on `pull_request`.
   - If Pro isn't active by the time 10a merges, this item moves to the start of 10b. Either way, it must be done before the first trial.
5. Run `make dev`, `make migrate`, and confirm the `localhost:5055` registry is serving the java21 image digest named in `deploy/local/sandboxes.yaml`.

## 10a — scope

### 1. Docs split (first commit)
- In `docs/tickets/milestone-1.md`, split Ticket 10 into 10a/10b and update the table and working order. Move the trials, `docs/trials/`, the 7/10 run and the live worker-kill to 10b.
- Write `docs/tickets/ticket-10-plan.md`: this plan, including the 10b section, kept in the repo the way the 7 and 8-pre plans were.
- In `docs/guide/tickets/README.md`, split row 10 into 10a and 10b.

### 2. Migration `00010_halts.sql` + store
- Add `halt_events (id, kind CHECK in ('set','clear'), reason, actor, occurred_at clock_timestamp())`. It is append-only, and `conclave_app` gets SELECT and INSERT only (same pattern as `cost_events`, 00003/00004). The current state is the latest event.
- Add `store.SetHalt`, `store.ClearHalt` and `store.HaltState(ctx) (HaltState{Halted bool, Since time.Time, ID})` in `internal/store/halt.go`.
- Tests: `TestHaltStateLatestEventWins`, `TestSetHaltTwiceStaysOne` (idempotent while halted), and an append-only grant test added to `TestAppRoleCannotAlterAppendOnlyRows`.

### 3. `internal/alert` (new package, `doc.go`: record then deliver one alert)
- `Alert{OccurrenceID, Key, Severity (halt|pause|digest), What, Where, Action, OwnerAction, Runbook}`. `Where` holds identifiers only: FR ID, workflow ID, repo full name, issue number. Never a body, diff, transcript or secret. A constructor validates this, and `github.Sanitize`/secret redaction runs on every string.
- `Log`: a JSON-lines file opened `O_WRONLY|O_APPEND|O_CREATE|O_NOFOLLOW`, mode 0600. It holds a mutex and fsyncs each line. `Append(alert)` comes **before** any delivery, and `AppendDelivery(occurrenceID, outcome)` records `delivered #n` or `failed: <redacted err>`. A retry may append the same occurrence twice, and readers de-duplicate by occurrence ID (documented). The runbook suggests `chflags uappnd` on macOS.
- `Raise(ctx, log, issues IssueClient, a)` handles delivery:
  - list open issues in the ops repo carrying the `conclave-alert` label;
  - match only App-authored issues (user ID equals `bot_user_id`) whose body holds the marker `<!-- conclave-alert-key: <key> -->`;
  - on a match, comment with an occurrence marker, skipping if `ListIssueComments` already shows that occurrence. Otherwise create the issue with labels `conclave-alert` and `severity:<s>`, assigned to the owner.
- GitHub is the dedupe store, so there is no alerts table, and a retry after a lost create finds the issue.
- Tests (fakes):
  - `TestLogWrittenBeforeDelivery` (delivery fails, and the log still holds the alert and the failure);
  - `TestRaiseDedupesOpenIssue`;
  - `TestRaiseReopensAfterClose` (a closed issue means a new issue);
  - `TestRaiseTwiceOneComment`;
  - `TestSpoofedMarkerIgnored` (a non-App issue carrying the marker);
  - `TestAlertCarriesNoSecretOrContent` (canary-format value and a body; attempts the leak);
  - `TestLogRefusesSymlink`.

### 4. `internal/github` additions
- `CreateIssue(repo, title, body, labels, assignees)`, `ListOpenIssuesByLabel(repo, label)` (paginated) and `GetIssue`. `CreateComment` and `ListIssueComments` are reused (`issues.go:38,158`). All of these go through the existing `writeToken` checks and outgoing-body sanitizing.
- Permission sets per purpose (`appauth.go:47`, `:252`, `:290`):
  - `tokenPermissions` stays for run repos;
  - a new `alertTokenPermissions = {issues: write, metadata: read}` applies when the repo is the configured alerts repo;
  - `checkTokenReply`/`checkPermission` check against the set that was requested.
- An alert token can never push, and no run token covers the ops repo (RL-18).
- Config: `deploy/github.yaml` gains an `alerts:` block (`repository`, `repository_id`, `assignee`), placeholders included. Validation refuses an alerts repo that is also in `repos.yaml`. Fill it in `deploy/local/github.yaml`.
- Tests:
  - `TestAlertTokenAsksOnlyIssues`;
  - `TestAlertTokenWithContentsRefused`;
  - `TestRunTokenPermissionsUnchanged`;
  - `TestLoadConfigRejectsAlertsRepoThatRuns`;
  - `TestCreateIssue`, `TestListOpenIssuesByLabelPaginates`;
  - leak tests extended to the new calls.

### 5. Pause alert from the workflow
- New activity `RaiseAlert(ctx, AlertInput{FeatureRequestID, Kind, WorkflowID})`. It loads the identifiers from the store and builds the alert there, so no text passes through history (RL-14). It then calls `alert.Raise`. The occurrence ID is the workflow ID plus the transition revision. Small options apply, and a failure is logged and never blocks.
- `featureRun.needsAttention` (`internal/workflow/feature.go:355`) calls it for `budget_cap` reasons. That covers both the task path (`feature.go:313`) and the pr_writer cap in delivery (`delivery.go:134`). The call sits behind `workflow.GetVersion("alerts-10", DefaultVersion, 1)`.
- Tests:
  - `TestBudgetCapRaisesPauseAlert` (one alert per stop, deduped on `/retry` then a second stop);
  - `TestRaiseAlertFailureStillWaits`;
  - `TestRaiseAlertTwice`;
  - the existing `TestReplayMidAttempt`/`TestReplayDetectsRerun` fixtures replay unchanged (NFR-R1 version gate).

### 6. Scheduled reaper (NFR-R3)
- `ReaperWorkflow(ctx, olderThan)` in `internal/workflow/reaper.go` runs one activity. `Activities.SweepOrphans` is the existing `SweepOrphans` wrapped as an activity method; it is idempotent and has `StartToClose` 10m.
- The worker ensures a Temporal Schedule `conclave-reaper` at startup: every `-reap-every` (default 15m), overlap SKIP, args = `-sweep-after`. If the schedule already exists, startup updates it. The startup sweep stays.
- Fold in ticket 9 open item: `checkSweepAfter` must also exceed Finalize's 45-minute bound (`cmd/worker/main.go:207`). `LongestAttempt` becomes "longest sandbox holder".
- Tests:
  - `TestReaperWorkflowSweeps` (testsuite);
  - `TestSweepOrphansActivityTwice`;
  - `TestEnsureReaperScheduleIdempotent` (fake schedule client);
  - `TestCheckSweepAfter` gains a Finalize case.

### 7. Kill switch: `cmd/conclave` (FR-SEC11)
New binary. Its subcommands are `halt -reason`, `resume [-keep]`, `alert-test` and `report`. It reads `CONCLAVE_DATABASE_URL` and the App key file, and unsets them (reusing `readSecrets`' pattern). Flags: `-github-config`, `-alert-log`, `-temporal-*`, `-drain` (default 3m). Ignore `/conclave` in `.gitignore`.

`halt` runs these steps in order, and each is idempotent so a second `halt` is safe:
1. Insert a `set` event (store), then append and deliver the halt alert. Delivery failure is logged and never stops the halt.
2. Pause the `conclave-reaper` schedule.
3. List running workflows (`ExecutionStatus = "Running"`) and cancel each. Re-list and re-cancel until none is open or `-drain` expires, so a label arriving mid-halt is caught too. Cleanup runs in the workers' disconnected contexts.
4. Sweep with no age cutoff: `SweepOrphans(olderThan=0)` through the Docker provider. Then verify that `UnreleasedAttempts(0)` is empty and that no container carries the `conclave.sandbox` label, and print both.
5. Edit the halt alert with the actions taken.

Workers (`cmd/worker`) get a halt watcher goroutine. It polls `HaltState` every 5s; on `set` it waits `-halt-grace` (default 3m, greater than the 2m heartbeat timeout so cancellations land), then cancels the serve context. Startup refuses while halted.

`resume`:
- cancels workflows started after the halt's `Since` (it lists them first, and `-keep` skips this);
- inserts a `clear` event;
- unpauses the schedule.

The Temporal calls go through a narrow local interface (List, Cancel, schedule Pause/Unpause), so unit tests use a fake.

Tests:
- `TestHaltOrder`, `TestHaltTwice`, `TestHaltCancelsLateStarts`, `TestHaltDeliveryFailureStillHalts`;
- `TestResumeCancelsStartedDuringHalt`, `TestResumeKeep`;
- `TestWorkerRefusesStartWhileHalted`, `TestWatcherStopsAfterGrace`;
- an integration test `TestHaltAgainstLiveTemporal`: testcontainers Temporal (`temporalio/temporal:1.9.1`, already in compose) plus Postgres. Real workers run fake activities and a fake sandbox provider. It asserts every workflow is cancelled, every sandbox released and the workers exit. The test runs in CI under `CONCLAVE_REQUIRE_INTEGRATION`; testcontainers-go is already a dependency, so nothing new is added.

### 8. NFR-R1: worker killed mid-attempt resumes on a second worker
- Integration test `TestWorkerKilledMidAttemptResumesOnSecond` in `internal/workflow`, using the same testcontainers Temporal. It follows the `startCaptureWorkers` pattern (`replay_test.go:263`).
- Two worker sets, A and B, poll both queues, with fake activities whose invocation counters are shared. A's `RunAttempt` blocks, then A is stopped.
- After the 2m heartbeat timeout, assert:
  - B continues the workflow;
  - every activity completed before the kill ran exactly once;
  - ordinal 1 settles `errored`, ordinal 2 runs on B, and the workflow completes.
- `MaximumAttempts: 1` means the attempt itself is not resumed. That is stated in the test name doc and the guide page.

### 9. Trial report query (seam for M2, exit-checklist item 3)
- `internal/store/trial_report.sql` (embedded) plus `store.TrialReport(ctx, db, from, to)`.
- One row per feature request: repo, issue, state, attempts, tokens, `cost_usd` summed from `cost_events` by `feature_request_id` (pr_writer included), approve→delivered time from `state_transitions`, and summed attempt time.
- Summary: resolve rate (delivered / trials, cancelled shown separately so either reading of the criterion works), cost per resolved task, and time per task.
- `conclave report` prints it.
- `TestTrialReport` seeds a delivered FR, a needs_attention FR and a cancelled FR through the 8-pre store functions, then checks each figure.
- Track `docs/trials/README.md` (owner-section issue format, log fields, how to run the report) and add `/docs/trials/local/` to `.gitignore`.

### 10. Docs
- `docs/runbooks.md`: sections for halt, budget-cap pause, alert delivery failed, and alert-test. Alert bodies link here. Also fix the duplicated "2." in the incident runbook (`security-and-threat-model.md:214`).
- `docs/decisions/0015-kill-switch-alerts-and-reaper.md`, `proposed`: CLI over `/halt`, an append-only halt table, grace before stopping workers, GitHub as the dedupe store, the private ops repo with an issues-only token, Schedule over a timer loop, and resume cancelling halt-time starts.
- `docs/decisions/0016-milestone-1-deferrals.md`, `proposed`: exit-checklist item 10's list, plus unknown-actor auto-halt, the weekly digest, canary alerts (Milestone 2) and a second alert channel. Add both rows to the README.
- Guide page `docs/guide/tickets/10a-kill-switch-alerts-and-reaper.md` from `_template.md`.
- In `m1-exit-checklist.md`, move FR-SEC11, FR-SEC31, NFR-R3 (reaper), NFR-R1 (two workers) and NFR-M9 to merged rows with the test names. The trials row goes to 10b.
- PR description: requirement → test mapping, RL-18 (the three self-check answers, mapped onto the ops-repo token, the server staying DB-less, and the halt not depending on GitHub), and the per-activity crash table for `RaiseAlert` and `SweepOrphans`.

No contract changes. No new dependencies: the Temporal SDK schedule client and testcontainers-go are already in `go.mod`.

## 10b — live trials (next session, outline only)

1. Do step 0. Re-run the ruleset check (`gh api repos/austmsk/ets/rules/branches/main`) before the first trial. Then start server, worker and tunnel with the `deploy/local/*` flags.
2. Write 10 ETS issues in owner-section format, from the candidates in the owner's local notes (ETS is private, so they are not listed here), saved in `docs/trials/local/`. Decide at the start whether the 2 cancellations are 2 of the 10 or extra runs.
3. For each trial: label, `/approve`, observe, record base SHA, attempt, gates, PR, CI, cost and duration. A `/retry` is not a new trial. Fix what breaks, with the two-failure circuit breaker, and each fix gets a test.
4. During one trial, kill the worker process mid-attempt and start a second worker (live NFR-R1).
5. Run `conclave halt` once with a run in flight, then `resume`. Run `conclave alert-test`.
6. After the trials, confirm `UnreleasedAttempts` and `docker ps --filter label=conclave.sandbox` are both empty. Run `conclave report`, and update the checklist's success-criteria rows and the 10b guide page.

## Verification (10a)

- `make check` (and `make check-ci`, so the testcontainers Temporal tests run).
- Manual: `make dev && make migrate`. Run the worker with the `deploy/local/*` flags, then confirm `temporal schedule list` shows `conclave-reaper` and its runs in the UI.
- Run `conclave alert-test`. An issue should open in the ops repo, and a second run should comment on that same issue. The line should appear in the alert log.
- Start a fake-provider run, run `conclave halt`, and check: workflows cancelled, worker exited, zero labelled containers. Then run `conclave resume`.
- After Pro is active: `gh api repos/austmsk/ets/rules/branches/main` shows the three rules, and the ruleset has no bypass actors (step 0, item 4).
- Then `/review-ticket 10a`, fix the findings, and open the PR.
