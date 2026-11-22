# 0003 — Milestone 1's database, migration, Docker and test dependencies, and sqlc deferred to Milestone 2

**Date:** 2026-11-21 (reserved at ticket 2, 2026-10; written after Milestone 1 closed)
**Status:** accepted

## Context

NFR-M4 and `CLAUDE.md` ask for a written reason behind every dependency: why the standard library is not enough, and why this module rather than another. Two of Milestone 1's direct dependencies have one: `go.yaml.in/yaml/v3` (0007) and the Temporal Go SDK (0012). Five do not. Tickets 2 and 3 added them before records were being written:

| Module | Added by | Used in | Reaches a binary |
|---|---|---|---|
| `github.com/jackc/pgx/v5` | ticket 2 | `internal/store`, `internal/activity`, `cmd/worker`, `cmd/conclave` | yes |
| `github.com/pressly/goose/v3` | ticket 2 | `internal/store` (`Migrate`), `make migrate` | linked, never called |
| `github.com/testcontainers/testcontainers-go` and `…/modules/postgres` | ticket 2 | tests, through `internal/testenv` | no |
| `github.com/moby/moby/client` and `…/api` | ticket 3 | `internal/sandbox/docker` | yes |
| `github.com/containerd/errdefs` | ticket 3 | `internal/sandbox/docker` | yes |

The same stack table also names `sqlc` for typed queries. It was never set up. Every query in the store, 54 of them across 16 files, is hand-written `pgx`. Meanwhile `CLAUDE.md`, `ARCHITECTURE.md` and NFR-M1 all describe `sqlc` as the project's one code generator, so a session reading them could reach for a tool that is not there.

## Decision

1. **`pgx` is the Postgres driver and pool.** `database/sql` with `lib/pq` was the alternative. `pgx` was chosen for four reasons:
   - `lib/pq` is in maintenance mode.
   - `pgx` returns `*pgconn.PgError` with the SQLSTATE and constraint name. `ApprovePlan` tells a second approved plan from other failures by the `23505` unique violation on `plans_one_approved_per_feature_request`. The tests assert the attacks fail the right way: `42501` permission denied, and Conclave's own `CO001` from the audit trigger.
   - It scans `uuid`, `timestamptz` and `jsonb` without adapters.
   - `pgx.Tx.Begin` opens a savepoint, so the halt functions work the same inside a caller's transaction, and the tests nest each attack in a savepoint they roll back.

   The store takes a small `DBTX` / `TxDB` interface, satisfied by both `*pgxpool.Pool` and `pgx.Tx`, so one function serves a pool, a transaction or a savepoint. The `database/sql` driver (`pgx/v5/stdlib`) is used only by tests, to give goose the `*sql.DB` it needs.

2. **`goose` runs the migrations.** The alternatives were `golang-migrate`, `atlas`, or a hand-written runner:
   - Goose migrations are plain SQL files, with `-- +goose StatementBegin` for the `DO` blocks and trigger functions the schema needs.
   - It runs as a library, with the migrations embedded (`store.Migrate`, `//go:embed migrations/*.sql`), so tests apply exactly the files that ship. It also runs as a CLI (`make migrate`).
   - Every migration has a tested `Down` (`TestMigrationsApplyAndRollBack`).
   - `atlas` is a schema-diffing tool, more than a fixed sequence of reviewed files needs. A hand-written runner would re-implement goose's version table and locking.

   The CLI's version is read from `go.mod`, so `make migrate` and the tests always run the same goose. Until now the `Makefile` pinned v3.28.0 while the library was at v3.29.0.

3. **`testcontainers-go` runs real Postgres and Docker in tests.** The store's guarantees are properties of Postgres, not Go: the audit trigger, the grants, `session_user`, compare-and-set under concurrency, advisory locks. A fake database would test none of them, and `CLAUDE.md` asks that security controls be tested by attempting the attack. Testcontainers starts the same image as `deploy/docker-compose.yml`, per package, and removes it afterwards. The alternative, a shared database that `make dev` brings up, would leak state between runs and make CI depend on a service it has to start by hand. Nothing in a binary imports it: `go list -deps` on `cmd/worker`, `cmd/server` and `cmd/conclave` finds no testcontainers package. Without Docker the suites fail rather than pass silently, and `-short` skips them.

4. **The Docker Engine's own Go client (`moby/moby/client` and `…/api`) drives the sandbox.** The alternatives were shelling out to the `docker` CLI, or writing the Engine API on `net/http` as was done for GitHub (0010) and the model providers (0008):
   - The CLI would make the sandbox's security settings (no network, dropped capabilities, `no-new-privileges`, limits, labels) a matter of argument strings and parsed output. The typed `HostConfig` is what `TestHostConfigIsHardened` checks.
   - The Engine API covers containers, networks, exec with stream demultiplexing, archive copies and events. That is far more surface than the GitHub REST calls Conclave makes, and the moby module is the API's own definition, with no vendor layer of the kind 0008 and 0010 avoid.
   - The module is the client released on its own from the moby repository, so the sandbox depends on the client and the API types, not the daemon's module.

5. **`containerd/errdefs` classifies the Engine's errors.** The moby client returns errors that implement containerd's error classes. `errdefs.IsNotFound` is how `Release` and `Reap` tell "already gone" (success, since cleanup is idempotent) from a real failure. Matching on error strings would break with an Engine upgrade.

6. **`sqlc` is deferred to Milestone 2.** For now:
   - The store is hand-written `pgx`, with every function tested against real Postgres with the real migrations applied, so a query that drifts from the schema fails `make check`.
   - `store.Transition` builds its `UPDATE` from a fixed map of table names. `sqlc` cannot parameterize an identifier, so that query, the centre of decision 0002, would stay hand-written whatever happens.

   `sqlc` remains the only code generator Conclave may adopt (NFR-M1). The first Milestone 2 ticket that adds a package of read-heavy queries, such as the benchmark's results and report store, decides whether to bring it in, for that package only, and records the decision. Until then, `CLAUDE.md`, `ARCHITECTURE.md` and NFR-M1 say it is deferred rather than in use. The `sqlc generate` permission in `.claude/settings.json` stays, ready for that ticket.

## Alternatives considered

- **Adopt `sqlc` now, for consistency with the stated stack.** It would add a pinned tool, a configuration file, committed generated code, a CI check that the generated code is current, and two styles in one package, since `Transition` cannot be generated. That is all cost with no failure behind it: the tests already catch schema drift.
- **Drop `sqlc` from the stack outright.** Milestone 2's benchmark and report add many queries returning wide rows. That is where generated scanning helps most, and where hand-written `Scan` calls are easiest to get wrong. Deferring keeps the option without the docs claiming it.
- **The alternatives for each module** are given with it above.

## Consequences

- **Every binary links goose.** `internal/store` holds `Migrate`, so `cmd/worker`, `cmd/server` and `cmd/conclave` all link goose and its SQL dialects, though none calls `Migrate`. The webhook server links `pgx` too, through the workflow package it starts runs with, though it never opens a database connection. Moving `Migrate` to its own package would remove both. That is a refactor for when binary size or the import graph matters, not now.
- **Testcontainers is heavy.** It requires 52 modules of its own, among them OpenTelemetry and Docker's packages, and much of the 68-line indirect block in `go.mod` is there for it. They stay out of the binaries, but `govulncheck` scans them, and a bump touches many lines of `go.sum`.
- **Tests need Docker.** Running the store, activity, sandbox and worker suites without Docker fails them; `-short` skips them. CI provides Docker.
- **Upgrades are deliberate.** Each of these modules is updated in its own change, with `make check` run before merging. The Temporal SDK 1.51 update showed why: it deprecated an API that one test used, and only the lint step caught it.
- **When Milestone 2 decides on `sqlc`,** that ticket's decision record updates this one, and `CLAUDE.md`, `ARCHITECTURE.md` and NFR-M1 change with it.
