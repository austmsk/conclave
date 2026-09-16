# Conclave

A single-owner platform that turns GitHub issues into reviewed pull requests: it reads the existing code, plans the change, implements and tests it in isolated sandboxes, gates the result, and opens a PR for a human to merge.

Multi-agent coordination, done by deterministic Go rather than by a manager agent.

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — the system and the decisions behind it
- [`docs/scope-and-requirements.md`](docs/scope-and-requirements.md) — scope, non-goals, requirements
- [`docs/security-and-threat-model.md`](docs/security-and-threat-model.md) — threat model and controls
- [`docs/agent-roles.md`](docs/agent-roles.md) — role contracts
- [`docs/review-list.md`](docs/review-list.md) — topics to understand in depth
- [`docs/tickets`](docs/tickets/) — the build plan
- [`docs/decisions`](docs/decisions/) — decision records

Status: pre-Milestone 1.
