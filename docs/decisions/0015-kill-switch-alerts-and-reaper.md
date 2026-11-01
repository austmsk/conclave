# 0015 — The kill switch, alerts and the scheduled reaper: a CLI halt in Postgres, alert issues in a private operations repository, and a Temporal schedule

**Date:** 2026-10-31
**Status:** proposed

## Context

Ticket 10a adds the three Milestone 1 controls that have no code yet: the kill switch (FR-SEC11), alerting (FR-SEC31), and the periodic orphan reaper (NFR-R3). The specification leaves most of the shape open:

- **The kill switch.** FR-SEC11 says "a single command cancels all in-flight runs and stops workers", but not what the command is, where the halt is recorded, or what happens to a label applied during one.
- **Alerts.** FR-SEC31 says alerts go to "the platform's repository", but never names it. Conclave's own repository is public and the Milestone 1 target, ETS, is private, so even a repository name in an alert would leak. It asks for a dedupe key without defining one, and for a local append-only file with no path.
- **The reaper.** NFR-R3 asks for a periodic reaper with no period or age, and until now the sweep ran only once, at worker startup.
- **What raises an alert in Milestone 1.** There are no canary tokens yet, and "repeated authorization failure from an unknown actor" has no threshold.

The owner decided, on 2026-10-31:

- The kill switch is a CLI, `conclave halt` and `conclave resume`.
- Alert issues go to a new private operations repository.
- The alert sources are the kill switch (`halt`) and a budget-cap stop (`pause`), plus `conclave alert-test`. The rest are deferred (decision 0016).
- The webhook server stays without database access, and `resume` cancels every workflow started during the halt.

## Decision

- **`conclave halt` is a command in a new `cmd/conclave` binary, not an issue comment.** It runs on the central machine with the database URL, the App key and Docker. In order, it:
  1. records the halt;
  2. raises the halt alert;
  3. pauses the reaper's schedule;
  4. cancels every open root workflow, listing again until two listings in a row find none open (Temporal's listing trails a start) or `-drain` (3 minutes) runs out;
  5. sweeps every sandbox with no age cutoff;
  6. counts unreleased attempts and `conclave.sandbox` containers;
  7. comments the result on the halt's issue.

  Each step can be repeated, so a second `halt` continues the first. The steps are in [`runbooks.md`](../runbooks.md#halt).
- **The halt lives in Postgres, in an append-only table.** Migration 00010 adds `halt_events`: a `set` or `clear` event with a reason, an actor and a time. The application role may select and insert, never update or delete, so a compromised worker cannot erase a halt or the record of one (threat T6). The current state is the event with the highest ID. `store.SetHalt` and `store.ClearHalt` take an advisory lock and record nothing when the state is already the one asked for. A second halt keeps the first one's time, which `resume` needs.
- **Workers stop themselves.** A worker polls `halt_events` every 5 seconds. On a halt it keeps running for `-halt-grace` (3 minutes) before it exits. That is longer than the attempt heartbeat timeout (2 minutes), so a cancelled attempt hears of the cancellation on a heartbeat, and the feature workflow's disconnected cleanup releases its sandbox while a worker is still there to run it. A worker refuses to start while the halt stands. The CLI never signals a worker process: the workers may be on another machine in later milestones, and the database is the one place every worker already reads.
- **The halt's sweep has no age cutoff.** `activity.SweepOrphans` runs with `olderThan` 0, which also removes a sandbox still in use. The scheduled sweep refuses a cutoff that low (below).
- **The server stays without database access, so `resume` cancels halt-time starts.** The webhook server cannot see the halt. A label applied during one starts a feature workflow that no worker runs. `conclave resume` lists every workflow started after the halt's time and cancels it unless the owner passes `-keep`. It then records a `clear` event and unpauses the schedule. Giving the internet-facing server database access, just to refuse those starts, would widen what a compromised server reaches (RL-18).
- **Alerts go to a private operations repository, with an issues-only token.** An `alerts:` block in `github.yaml` names the repository, its numeric ID and the owner's login to assign. It must be in the App's one installation.
  - A token for it asks for `issues: write` and `metadata: read` only (`alertTokenPermissions`). A reply granting more, `contents` included, is discarded unused.
  - The alert calls (`CreateAlertIssue`, `ListOpenAlertIssues`, `CommentOnAlertIssue`, `AlertIssueComments`) reach no other repository.
  - The worker refuses to start if the alerts repository is also in `repos.yaml`, so no agent ever works where alerts are read.
  - The worker and the CLI require the block (`RequireAlerts`). The server may leave it out, but a partly filled block fails validation everywhere.
- **The local log comes first.** `alert.Raise` appends an `alert` entry to a JSON-lines file before it tries delivery, then a `delivery` entry with the outcome. A log write that fails stops delivery: an alert the local record does not hold is never sent.
  - The file is opened append-only, mode 0600, never through a symlink, and each line is one write followed by an fsync.
  - Making it append-only for its owner too (`chflags uappnd`, `chattr +a`) is left to the operating system and the runbook.
- **GitHub is the dedupe store.** Each alert has a key, the condition, and an occurrence ID, this instance of it. The issue body ends with two plain lines: `conclave-alert-key: <key>` and `conclave-alert-occurrence: <id>`.
  - Delivery lists the open issues labelled `conclave-alert` and keeps only those the App's bot opened.
  - It matches the key line as a whole line, comments on that issue unless the issue or one of the App's comments already holds the occurrence, and otherwise opens a new issue.
  - So a retry after a lost create or comment posts nothing twice, and once the owner closes an issue the next occurrence opens a new one.
  - The lines are visible text rather than HTML comments, because `sanitizeMarkdown` strips HTML comments from every outgoing body.
- **Alert text is built from identifiers.** Each constructor (`Halt`, `HaltDone`, `BudgetCap`, `Test`) takes typed IDs and writes the text itself: what happened, where, what was already done, what the owner must do, and a runbook link.
  - `Where` values must look like identifiers.
  - The one free-text field, the halt reason, is redacted for token shapes, made one line and cut at 200 characters.
  - The run's issue is written as `` `owner/name` issue N ``, not `owner/name#N`, so GitHub does not cross-link it.
- **The pause alert comes from a budget-cap stop only.** `featureRun.needsAttention` raises it after a successful move whose reason is `budget_cap`. That covers both the task path and the pr_writer's cap in delivery.
  - It runs through the `RaiseAlert` activity, which reads the repository and issue from the database, so no alert text passes through history (RL-14).
  - The occurrence is the feature request plus its revision after the move, so each stop is its own occurrence and a stop after `/retry` comments on the open issue.
  - The activity gives up after 10 minutes and its failure is logged. The stop is still on the issue's status comment, so a GitHub outage never holds the `needs_attention` wait.
  - Other stops raise no alert: FR-SEC31 excludes conditions already visible elsewhere, and every stop is on the status comment.
- **A version gate.** `FeatureWorkflow` reads `GetVersion("alerts-10", DefaultVersion, 1)` at its start, as 8b and 9 do. A run started before this ticket raises no alert, so its history still replays.
- **The reaper runs on a Temporal schedule.** At startup the worker creates or updates the schedule `conclave-reaper`.
  - The schedule starts `ReaperWorkflow` every `-reap-every` (15 minutes, at least 1), with overlap SKIP.
  - Its argument is the `-sweep-after` cutoff (2 hours by default).
  - An update keeps the schedule's paused state, so a restarted worker cannot undo a halt's pause.
  - `ReaperWorkflow` runs the `SweepOrphans` activity, up to three tries; the next scheduled run is the retry after that.
- **The sweep cutoff covers Finalize.** The cutoff must exceed `Deps.LongestSandboxHold`, the longest attempt or Finalize's 45 minutes (`activity.FinalizeTimeout`), whichever is longer.
  - The worker refuses a lower `-sweep-after` at startup.
  - The `SweepOrphans` activity refuses one too, so a schedule edited by hand cannot remove a live sandbox.
  - This closes the ticket 9 review item that `checkSweepAfter` ignored Finalize.

## Alternatives considered

- **A `/halt` issue comment.** Rejected: it arrives through the webhook, so it fails exactly when GitHub or the owner's account is the problem (threat S3). It would also need a new command in the `commands` table's check and the command table.
- **The halt in Temporal**, as a signal to every workflow or a search attribute. Rejected: it does not stop a worker, it does not hold while Temporal is unreachable, and it leaves no append-only record.
- **The CLI stopping workers directly**, by signal or process ID. Rejected: it assumes one machine, and a worker killed at once cannot run the cleanup its cancellations need.
- **Database access for the webhook server**, so it can refuse a label during a halt. Rejected for RL-18: the internet-facing process holds only the webhook secret today.
- **Alerts in Conclave's public repository**, naming repositories by numeric ID. Rejected: the owner's notifications would land in a public tracker, and IDs alone make alerts hard to act on.
- **An `alerts` table for deduplication.** Rejected: it needs a write after the GitHub call, which a crash can lose, and the issue list already answers whether the alert is open. GitHub's search API was also rejected, because it is eventually consistent and the issue list is not.
- **A long-running reaper workflow with a timer loop and continue-as-new.** Rejected: a schedule is Temporal's own mechanism, can be paused by a halt, and needs no history management.

## Consequences

- **Manual owner steps:**
  - Create the private operations repository and add it to the App's installation.
  - Fill `alerts:` in `deploy/local/github.yaml`. Until then the worker and the CLI refuse to start, and so does the server, since the block is partly filled.
  - Pass `-alert-log` to the worker and the CLI.
  - Optionally make the log append-only with `chflags` or `chattr`.
- **A halt removes in-use sandboxes.** An attempt running when the halt sweeps loses its sandbox and ends `errored`. Its spend is kept.
- **A workflow can outlast a halt.** If its cleanup has not finished before the workers stop, it stays open with a pending activity and finishes when workers start after `resume`. The sweep has already released its sandbox.
- **The halt depends on Postgres.** With the database down, workers neither start nor do work, and `conclave halt` cannot record a halt. Stopping the worker processes by hand is then the fallback.
- **A retried delivery can write an occurrence to the log twice.** Readers group by occurrence ID.
- **Alerts that are not delivered are not batched.** The `digest` severity is delivered at once in Milestone 1. The weekly batch, unknown-actor auto-halt, canary alerts and a second channel are deferred (decision 0016).
