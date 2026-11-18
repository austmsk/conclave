# Conclave

A single-owner platform that turns GitHub issues into reviewed pull requests: it reads the existing code, plans the change, implements and tests it in isolated sandboxes, gates the result, and opens a PR for a human to merge.

Multi-agent coordination, done by deterministic Go rather than by a manager agent.

- [`docs/guide`](docs/guide/) — how the system works, ticket by ticket; start here
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — the system and the decisions behind it
- [`docs/scope-and-requirements.md`](docs/scope-and-requirements.md) — scope, non-goals, requirements
- [`docs/security-and-threat-model.md`](docs/security-and-threat-model.md) — threat model and controls
- [`docs/agent-roles.md`](docs/agent-roles.md) — role contracts
- [`docs/review-list.md`](docs/review-list.md) — topics to understand in depth
- [`docs/tickets`](docs/tickets/) — the build plan
- [`docs/decisions`](docs/decisions/) — decision records

## Status

Milestone 1 (walking skeleton) is complete. All fourteen tickets are merged ([`docs/tickets/m1-tickets/milestone-1.md`](docs/tickets/m1-tickets/milestone-1.md)), and a labelled issue on the Election Tally repository now becomes a pull request that passes its CI: 7 of 10 live trials delivered, with no orphaned sandboxes across them. The exit checklist ([`m1-exit-checklist.md`](docs/tickets/m1-tickets/m1-exit-checklist.md)) maps each requirement to the test that proves it and lists what Milestone 2 starts from. What Milestone 1 defers is in [decision 0016](docs/decisions/0016-milestone-1-deferrals.md).

Milestone 2, the evaluation harness, is being designed in [`docs/tickets/m2-tickets`](docs/tickets/m2-tickets/).

## Development

Requires Go (the toolchain pinned in `go.mod`) and Docker. The sandbox tests need a Docker daemon, and the store tests start Postgres in one through `testcontainers-go`; without a daemon they skip locally and fail in CI.

```
make dev       # Temporal dev server, Postgres with pgvector, Jaeger
make migrate   # apply migrations to the dev database
make check     # what must pass before a change is done
make down      # stop the stack
```

Temporal's UI is on `localhost:8233` and Jaeger's on `localhost:16686`.

Owner configuration lives in `deploy/`: `router.yaml` (models, prices, routes, rate limits) and `repos.yaml` (each repository's sensitivity tier and role allowlist). Both ship with placeholders that fail validation on purpose, so the router refuses them until real values are filled in.

To run the whole platform locally (worker, webhook server and tunnel), follow [`docs/running-locally.md`](docs/running-locally.md).
