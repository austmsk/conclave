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

Milestone 1 (walking skeleton) is in progress. Tickets 1–4 are merged: the repository skeleton and CI, the contracts package and schema, the Docker sandbox, and the LLM router. The working order for the rest is in [`docs/tickets/milestone-1.md`](docs/tickets/milestone-1.md). Nothing runs end to end yet; `cmd/server` and `cmd/worker` are placeholders until tickets 6 and 8.

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
