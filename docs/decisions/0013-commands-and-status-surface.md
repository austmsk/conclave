# 0013 — Commands and the status surface: one table, a closed-run workflow, and a versioned change

**Date:** 2026-10-31
**Status:** proposed

## Context

Ticket 8 left every problem routed to `needs_attention`, with only `/approve` and `/cancel` handled. Every other command was logged and dropped. Ticket 8b gives the owner the lifecycle they see (RL-9): `/retry`, a reply to a command that does not fit, `/status` and `/help`, one status comment, and an audit row per command (FR-S11, FR-S12, FR-SEC10). Five choices had to be made:

- **Where the valid-command rule lives.** The gate, the `/help` reply and the status comment's "commands available" each need it.
- **How a command reaches a closed run.** In `delivered` and `cancelled` the feature workflow has ended and cannot take a signal, and `cmd/server` holds neither database access nor the App key.
- **How an in-flight ticket-8 run survives the deploy.** Decision 0012 said 8b's first change would use `workflow.GetVersion`.
- **What the audit covers.**
- **What "still over a cap" means for `/retry`, and where the cap comes from.**

The owner decided, on 2026-10-30:

- `/help` joins the command table in all four live statuses.
- `/revise` and `/ask` reply "not available in Milestone 1".
- Terminal statuses are handled by a short workflow on the worker.
- The audit covers every owner command that dispatches.

## Decision

- **One command table.** `workflow.validCommands(status)` mirrors the `ARCHITECTURE.md` table plus `/help`, as a fixed, ordered slice per status. The signal gate (`commandApplies`), the `/help` reply and the status comment all read it, so they cannot disagree. `TestCommandApplies` checks all seven commands in all six statuses against a copy of the table written out in the test. `/revise` and `/ask` pass the gate and are then answered "not available in Milestone 1" (`not_available`).
- **A closed-run workflow, `ClosedCommandWorkflow`.** When `SignalCommand` returns `ErrNoWorkflow`, the dispatcher calls the new `WorkflowStarter.StartClosedCommand`.
  - It starts the workflow under `command/<repoID>/<issue>/<commentID>` with REJECT_DUPLICATE, so a redelivered comment is answered once.
  - The workflow runs on the worker. It looks up the feature request by `feature_requests.workflow_id`, replies, and records the command as rejected: `closed_run`, `not_available` for `/revise` on `delivered`, `no_run`, or `no_workflow` (live status, nothing running).
  - A comment already in the audit was decided by its live run, so it is left alone.
  - The server keeps no database access and no App key.
- **A version gate.** `FeatureWorkflow` reads `GetVersion("commands-8b", DefaultVersion, 1)` once, at the start.
  - At `DefaultVersion` (a run started before 8b) it behaves exactly as ticket 8 did: only `/approve` and `/cancel` act, and there is no reply, no audit and no status comment.
  - The unchanged fixtures `testdata/feature-mid-attempt.json` and `task-mid-attempt.json` still replay, which proves it.
  - A task child resumed by `/retry` gets the ID `<wf>/task/<taskID>/<ordinal>`. The first run keeps ticket 8's ID.
- **A responder coroutine.** Signal handlers decide synchronously, against the status at receipt, and append to an outbox.
  - One `workflow.Go` coroutine, on a disconnected context, sends each item in order: `RecordCommand`, `ReplyToCommand`, or `UpsertStatusComment` (a status refresh superseded by a later one is skipped).
  - The main flow never waits on GitHub for a response, and a response never routes the run to `needs_attention`.
  - Every return of the workflow waits for the outbox to drain first, so an accepted `/cancel`, or a command just before a Temporal cancel, has its row.
  - Response activities have a 10-minute schedule-to-close bound, not the run steps' hour.
- **The audit scope.** Every owner command that dispatches gets one row in the append-only `commands` table (migration 00008, keyed on comment ID with `ON CONFLICT DO NOTHING`). The row holds the outcome, the reason, and the status it was decided in.
  - Commands from anyone else, malformed commands and redeliveries stay log-only, as in ticket 6.
  - A `/retry` is audited after its cap check, so the row records the final decision.
  - An `/approve` or `/cancel` that arrives while one is already pending is recorded as rejected `already_pending`.
- **Cap re-check semantics.** `CheckCaps` reads `CostLedger.Spend` (settled plus live reservations) for the feature request and the month. It compares that spend with `Deps.RouterConfig.Budgets`, which is the configuration as of worker startup.
  - Spend **at or over** a cap counts as exceeded, because the router admits no call there.
  - The feature request cap is named before the monthly one.
  - The owner raises a cap by editing `deploy/router.yaml` and restarting the worker.
- **The status comment is one comment per feature request.** It is found by the marker `conclave-status:<id>` among the App's own comments, then edited or created. The workflow refreshes it after every feature-request move, and after a `/retry` refused for a cap.

## Consequences

- **A cap change is not recorded per run.** The plan body records the SHA-256 of `router.yaml` at planning time. After a raised cap and a restart, attempts run under a `router.yaml` whose digest is not the one in their plan. The cap is not per-run configuration, so this is accepted. Ticket 10's trial report should note any cap change.
- **Some edge cases stay log-only.** A command arriving in the instant between the drain and the workflow's completion is not audited. The receiver's log keeps it, and a redelivery reaches the closed-run workflow.
- **Ticket numbering shifts.** This record and migration 00008 are taken, so ticket 9's draft moves to migration 00009 and decision 0014.
- **Replay fixtures.** A v8b history has not been captured as a replay fixture. The fixtures that exist prove the old path still replays.
