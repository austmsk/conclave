# Ticket 2 — Contracts package and schema

**Status:** merged in `f7856c6`, with review fixes in `721ed17` and `7a25a50` · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-2--contracts-package-and-schema) · **Requirements:** FR-SEC10, NFR-M7 · **Review list:** RL-4, RL-13

## In one paragraph

The vocabulary and the database. `internal/contracts` defines the types every other package shares (feature requests, plans, tasks, attempts, roles, sandboxes, model requests) and the table of legal state changes. `internal/store` holds the Postgres schema as goose migrations, plus `Transition`, the only function that changes a record's state. Every state change is conditional on what the caller last read, and the database itself writes an audit row for it.

## Why it exists

Everything else is built behind these types, so they had to come first and be reviewed by hand. That is why `CLAUDE.md` forbids changing `internal/contracts` as a side effect.

The database design answers two questions that Temporal retries make hard:

1. **"Did my update land?"** An activity can update a row, crash before reporting success, and run again. The second run must recognise its own earlier work, not treat it as a conflict (`RL-13`).
2. **"Who changed this?"** Every state change needs an audit row naming its cause (`FR-SEC10`), and a compromised worker mustn't be able to skip or rewrite it.

## How it works

**The transition table.** `contracts/state.go` has one map per record kind, listing each state's legal next states. Terminal states map to an empty list. `CanTransition(kind, from, to)` is the only legality check anywhere. Self-transitions are illegal, because "already done" is detected differently (below).

**Compare-and-set.** `store.Transition(ctx, tx, req)`:

1. Checks the move against the transition table, before any SQL.
2. Sets `conclave.actor` on the transaction with `set_config(..., true)`.
3. Runs `UPDATE <table> SET state = $to, reason = $reason WHERE id = $id AND state = $from AND revision = $rev`.
4. If no row changed, re-reads the record and classifies the result:
   - not there → `ErrNotFound`
   - at the target state, exactly one revision on → `ErrAlreadyApplied`: my earlier try landed, so carry on
   - anything else → `*WrongStateError`: someone else moved it, so reconsider

**Why `revision`, not just `state`.** Tasks and feature requests can cycle: `running → needs_attention → running`. If a retry from the first `running` matched on state alone, it could land on the second `running` and apply a stale decision. The revision counter makes each pass round the cycle distinct.

**The audit trigger.** `record_state_transition` (in `00003_accounting.sql`) fires before any update of a `state` column. It:

- refuses the change with SQLSTATE `CO001` if no actor is set, so a raw `UPDATE` that bypasses `Transition` fails
- bumps `revision`
- inserts the audit row into `public.state_transitions`, including `db_user` from `session_user` (the authenticated login, which the app can't fake)

Because the trigger writes the row, the audit can't be forgotten, and it exists exactly when the change commits.

**The temp-table bypass, closed.** A temporary table named `state_transitions` would be found before `public` on the search path, so audit rows could be diverted into it. The trigger function pins `search_path = pg_catalog, public, pg_temp` and names `public.state_transitions` explicitly. `00004_app_role.sql` also revokes `TEMP` from `PUBLIC`. This was found in the second review round.

**Approving a plan.** `Transition` refuses to move a plan to `approved`. `ApprovePlan` pins `base_sha` and `approved_at`, conditional on state and revision so a retry can't overwrite the first SHA, then transitions. A partial unique index (`plans_one_approved_per_feature_request`) allows only one approved plan per feature request. A second approval returns `ErrAnotherPlanApproved`.

**The application role.** `conclave_app` gets SELECT, INSERT and UPDATE on ordinary tables, only SELECT and INSERT on `cost_events` and `state_transitions`, and DELETE on nothing. This is only a control if the app logs in as a non-owner member of `conclave_app`. An owner connection can `RESET ROLE` and escape it (see `store/doc.go`).

**Schema conventions** (top of `00001_work.sql`): UUID keys that callers may supply, so a retried insert can be `ON CONFLICT DO NOTHING`. Every state table has `state`, `reason` and `revision` columns, so one UPDATE works for all of them. Allowed values are CHECK constraints rather than enum types, so a later milestone can add a status with a plain `ALTER`.

## Where to look

| File | What's in it |
|---|---|
| `internal/contracts/state.go` | Statuses and the transition table |
| `internal/contracts/record.go`, `attempt.go`, `task.go` | Database record types |
| `internal/contracts/sandbox.go`, `llm.go`, `role.go`, `tool.go` | Interfaces for later tickets |
| `internal/store/transition.go` | `Transition`, `ApprovePlan`, error types |
| `internal/store/migrations/00001_work.sql` | Work tables and the one-approved-plan index |
| `internal/store/migrations/00002_evidence.sql` | `gate_results`, `decisions` |
| `internal/store/migrations/00003_accounting.sql` | `cost_events`, `state_transitions`, the audit trigger |
| `internal/store/migrations/00004_app_role.sql` | `conclave_app` grants, TEMP revoke |

## What the tests prove

| Guarantee | Test |
|---|---|
| The table matches the documented lifecycles; every pair of states is checked | `TestStatesMatchDocumentedLifecycle`, `TestCanTransitionExhaustive` |
| The schema's CHECK constraints accept exactly the contract's states | `TestSchemaAcceptsExactlyTheContractStates` |
| Every legal move works through the store | `TestTransitionCoversEveryLegalMove` |
| A replayed transition is `ErrAlreadyApplied`, not a conflict | `TestTransitionReplayInOneTransaction` |
| A stale retry on a cyclic lifecycle is refused | `TestTransitionStaleRetryOnCyclicLifecycle` |
| Concurrent updates classify correctly | `TestTransitionUnderConcurrentUpdate`, `TestApprovePlanUnderConcurrentApproval` |
| A raw UPDATE without an actor fails; temp tables can't divert audit rows | `TestAuditTriggerResistsBypass`, `TestTempTableCannotShadowAuditTable` |
| The app role can't update or delete append-only rows | `TestAppRoleCannotAlterAppendOnlyRows` |
| Only one approved plan per feature request | `TestOneApprovedPlanPerFeatureRequest` |
| Migrations apply and roll back | `TestMigrationsApplyAndRollBack` |

The store tests run against real Postgres via `testcontainers-go`.

## Left open

- Decision records 0002 (audit trail and transitions) and 0003 (pgx, goose, testcontainers-go) are reserved but not yet written.
- The `actor` string is self-reported. Only `db_user` is authenticated.
- `00004` grants table-wide UPDATE on `repos`, so the app role can change `repos.tier`. Ticket 4 made the config file authoritative and refuses a mismatch, but narrowing the grant is still undecided (see [ticket 4](04-llm-router.md)).
- Typed queries via `sqlc` aren't set up yet. The store code so far is hand-written `pgx`.

## Check your understanding

1. A `Transition` call updates zero rows. What three things could have happened, and how does the code tell them apart?
2. Why is matching on `state` alone unsafe for tasks but would be safe for attempts?
3. Why is the audit row written by a trigger rather than by `Transition`?
4. How could a non-owner role have diverted audit rows, and what two things stop it?
5. Why does approving a plan need its own function?
6. What deployment mistake would make the `conclave_app` grants useless?
