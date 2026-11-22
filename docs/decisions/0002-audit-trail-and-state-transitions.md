# 0002 — State changes and the audit trail: compare-and-set in Go, audit rows and logins written by the database, and one database role per process

**Date:** 2026-11-21 (reserved at ticket 2, 2026-10; written after Milestone 1 closed)
**Status:** accepted

## Context

Every Conclave record with a lifecycle (feature requests, plans, tasks, attempts) is changed by Temporal activities, and Temporal retries them. An activity can update a row, crash before it reports success, and run again. Two questions follow:

1. **Did my update land?** A retry must recognise its own earlier write, not report it as a conflict, and must never apply a stale decision (RL-13).
2. **Who changed this?** FR-SEC10 asks for an audit row for every state transition, and threat T6 asks that a compromised worker cannot skip or rewrite it.

Ticket 2 answered both, and every later ticket reused the answer, adding append-only tables one at a time: `tool_calls` (ticket 5), `commands` (8b), `pr_ci_results` and column-level grants on `pull_requests` (9), and `halt_events` (10a). None of that was written down as a rule, and a review after Milestone 1 found three gaps in it:

- **Nothing enforced the login.** The grants are a control only if a process logs in as a non-owner member of the application role. That was documented, but a worker started with the owner's URL ran normally with every grant void.
- **The worker's login could lift a halt.** Workers only read `halt_events`, but `conclave_app` could insert into it, and the CLI connected through the same role. So the worker's database URL could clear a halt with a hand-written row, past the CLI's terminal check, and the row would read `actor = 'cli'` like the owner's own.
- **Only one table recorded who connected.** `state_transitions` records `db_user`, the authenticated login. `halt_events` and `commands` recorded only the self-reported `actor`.

## Decision

1. **One legality table, checked before any SQL.** `contracts/state.go` lists each state's legal next states, and `CanTransition` is the only legality check. A move to the same state is illegal; "already done" is detected by the next rule instead.

2. **Compare-and-set on state and revision.** `store.Transition` runs `UPDATE … WHERE id = $id AND state = $from AND revision = $rev`. When no row changes, it reads the row again and classifies the result:
   - not there: `ErrNotFound`;
   - at the target state, exactly one revision on: `ErrAlreadyApplied`, so the retry carries on;
   - anything else: `*WrongStateError`, so the caller reconsiders.

   The revision matters because tasks and feature requests cycle (`running → needs_attention → running`). A retry matching on state alone could land on the second `running` and apply a decision made for the first.

3. **The database writes the audit row.** The trigger `record_state_transition` (migration 00003) fires on any update of a `state` column. It refuses the change with SQLSTATE `CO001` unless the transaction has set `conclave.actor`, bumps `revision`, and inserts the row into `public.state_transitions` with `db_user` taken from `session_user`. The audit row therefore exists exactly when the change commits, and no code path can forget it. The trigger pins `search_path = pg_catalog, public, pg_temp` and names its target schema-qualified, and migration 00004 revokes `TEMP` from `PUBLIC`. Together these close the one diversion found in review: a temporary table named `state_transitions`, which would otherwise sit first on the search path.

4. **Approving a plan has its own function.** `Transition` refuses to move a plan to `approved`. `ApprovePlan` pins `base_sha` and `approved_at`, conditional on state and revision so a retry cannot replace the first SHA. A partial unique index allows one approved plan per feature request.

5. **Append-only is a grant, and the grant is the rule.** A table whose rows are evidence (cost, transitions, tool calls, commands, halts, CI results) gets `SELECT, INSERT` and nothing else. A table whose rows must change in a fixed way gets column-level `UPDATE` (`pull_requests`) or a revoke (`gate_results`, migration 00007). Migration 00004 sets no default privileges, so a new table grants exactly what it needs and never inherits `UPDATE` by accident. No role but the owner can delete a work row.

6. **One database role per process.** Migration 00011 adds `conclave_operator`, the CLI's role:
   - It is a member of `conclave_app`, so it can do what the CLI's sweep and report need.
   - It alone may insert into `halt_events`. `conclave_app` keeps `SELECT` and loses `INSERT`, so a compromised worker, or anyone holding the worker's URL, cannot lift a halt.
   - The CLI reads its own URL, `CONCLAVE_OPERATOR_DATABASE_URL`, which is never loaded where the worker runs.

7. **Every process checks its login at startup.** `store.CurrentLogin` reads `session_user` and the login's memberships. `CheckWorker` refuses the owner, a superuser, a member of the owner, an operator, and anything outside `conclave_app`. `CheckOperator` refuses the first three and anything outside `conclave_operator`. Each refusal names the login, never its password. The worker logs the login it runs as.
   - `conclave halt` warns about a wrong login and tries anyway: a kill switch must not wait on configuration, and the database still refuses a halt from a login that cannot record one.
   - `resume` and `report` refuse a wrong login.

8. **The authenticated login is recorded on halt and command rows.** Migration 00011 adds `db_user` to `halt_events` and `commands`. A `BEFORE INSERT` trigger, `record_db_user`, sets it from `session_user` and overwrites any value the insert supplies, so a login cannot claim to be another. A column default would not do this, since a caller who names the column replaces it. Rows from before the migration read `unrecorded`. `HaltState.DBUser` carries the value, and `conclave halt` and `resume` print it, as does the worker when it refuses to start during a halt.

9. **`repos.tier` stays writable by the application role.** `EnsureRepo` rewrites a repository's tier and default branch from the owner's configuration at every worker start (FR-SEC12), so narrowing the grant would break that. The configuration is the authority: the router compares the stored tier with the configured one on every request and refuses a mismatch (ticket 4, `TestRetieredRowIsRefused`).

10. **The development owner password is accepted while it is local.** `deploy/docker-compose.yml` and the `Makefile` use `conclave`/`conclave` for the owner, and the ports are published on `127.0.0.1` only. A deployment reachable from another machine must use a real owner password and keep it off every application host.

## Alternatives considered

- **Writing the audit row in Go, inside `Transition`.** Any path that bypassed `Transition` would leave no row, and a compromised process could simply not write it. The trigger makes the row a property of the database.
- **Compare-and-set on state alone.** Unsafe on cyclic lifecycles, as above.
- **Postgres enum types for states.** CHECK constraints let a later milestone add a status with a plain `ALTER`, and `TestSchemaAcceptsExactlyTheContractStates` keeps them equal to the contract.
- **Recording identity in `actor` alone, or with a column default.** `actor` is self-reported, and a default is replaced by any insert that names the column. Only a trigger reading `session_user` records what Postgres authenticated.
- **`db_user` without splitting the roles.** The CLI and the worker shared a login, so `db_user` would have recorded the same name for the owner's resume and a forged one. It verifies a login only when each process has its own.
- **Refusing `halt` on a wrong login.** Safer configuration, but it makes the kill switch fail for a reason unrelated to the emergency.
- **Narrowing `repos` to column grants.** See decision 9.

## Consequences

- **Proving tests.** The tests attempt each attack against real Postgres:
  - store `TestTransitionCoversEveryLegalMove`, `TestTransitionReplayInOneTransaction`, `TestTransitionStaleRetryOnCyclicLifecycle`, `TestTransitionUnderConcurrentUpdate`, `TestApprovePlanUnderConcurrentApproval`, `TestOneApprovedPlanPerFeatureRequest`;
  - store `TestAuditTriggerResistsBypass`, `TestTempTableCannotShadowAuditTable`, `TestAppRoleCannotAlterAppendOnlyRows`, `TestOperatorRoleHaltEventsAppendOnly`;
  - store `TestCurrentLoginChecks`, which connects as the owner, a member of the owner, an app login, an operator login and a login with no role;
  - store `TestAttackAppLoginCannotLiftHalt`, `TestAttackForgedDBUserOverwritten`, `TestOperatorHaltRecordsLogin`;
  - contracts `TestCanTransitionExhaustive`, `TestSchemaAcceptsExactlyTheContractStates`.
- **Deployment needs two logins.** One for the worker, in `conclave_app`, and one for the CLI, in `conclave_operator`, each in its own secrets file (`docs/running-locally.md`, step 3). An existing setup whose CLI used the worker's URL has to create the operator login before the next `conclave halt`: until then the halt warns and the database refuses it.
- **What `db_user` cannot do.** It names a login, not the person or process using it. Anyone holding the operator password looks like the owner, which is why `resume` also needs a terminal and raises an alert on the halt's issue (decision 0015). The owner can still disable triggers and rewrite rows, so `db_user` is only as trustworthy as the owner password's secrecy.
- **`actor` stays self-reported.** It names the cause (`workflow:<id>`, `github:<user id>`, `cli`), and `db_user` names the login. Neither alone says both.
- **Left for later.**
  - `db_user` on `cost_events`, `tool_calls` and `pr_ci_results`, which today only the worker writes.
  - An audit row for a change to `repos.tier`.
  - Authentication on Temporal before a second machine joins (threat model, B6): a client that can reach Temporal can signal a workflow past the owner check.
