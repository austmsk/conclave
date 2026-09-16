# Conclave — Instructions for Claude Code

Conclave is a single-owner platform that turns GitHub issues into reviewed pull requests by running agents in isolated sandboxes. Written in Go, orchestrated by Temporal.

Read [`ARCHITECTURE.md`](ARCHITECTURE.md) before making design decisions. Requirement IDs (`FR-*`, `NFR-*`) live in [`docs/scope-and-requirements.md`](docs/scope-and-requirements.md); security rationale in [`docs/security-and-threat-model.md`](docs/security-and-threat-model.md); role definitions in [`docs/agent-roles.md`](docs/agent-roles.md).

## Session discipline

- **One ticket per session.** Tickets are in `docs/tickets`. Start a new session for a new ticket rather than continuing a long one.
- **Circuit breaker.** After two failed attempts at the same error, stop and report what you tried and what you observed. Do not keep guessing.
- **Ask before widening scope.** If a ticket appears to require changes outside its stated files, say so and stop.

## Commands

```
make build        # go build ./...
make test         # go test -race ./...
make lint         # golangci-lint run
make check        # vet + lint + test + workflowcheck + govulncheck
make migrate      # goose up
make dev          # docker compose up (Temporal, Postgres, Jaeger)
```

`make check` must pass before a ticket is complete. Run it yourself; do not report a ticket as done on the basis that the code looks correct.

## Hard rules

**Temporal determinism.** In `internal/workflow/`: no `time.Now()`, no `math/rand`, no raw goroutines, no network calls, no direct database access. Use `workflow.Now()`, `workflow.Go()`, `workflow.NewSelector()`. Never range over a map — sort keys first, because Go randomizes map iteration order and replay will diverge. All side effects go in `internal/activity/`.

**Idempotent activities.** Every activity must be safe to run twice. A retry after a successful-but-unreported side effect must not open a second pull request, push twice, or record cost twice.

**Cleanup survives cancellation.** Sandbox cleanup runs in a deferred function using `workflow.NewDisconnectedContext`. A cleanup activity started with the workflow's own context will not run when the workflow is cancelled.

**Secrets.** Never log, trace, or persist credentials. Never read `.env` files or anything under `~/.config/conclave/`. Never write a real key into a test, fixture, or example — use the canary format (`canary_*`).

**Contracts are reviewed by hand.** Do not modify `internal/contracts` as a side effect of another change. If a ticket seems to require a contract change, describe the change and stop.

**No new dependencies without asking.** Propose the dependency and why the standard library is insufficient, then wait.

**Sandbox safety.** Never mount the Docker socket into a container. Never inherit the worker's environment into a sandbox. Never grant a sandbox network access during the agent phase.

## Style

Follow the maintainability requirements (`NFR-M1` through `NFR-M9`). In short:

- Standard library first. No web framework, no DI container, no reflection-based magic. `sqlc` is the only code generation.
- Early returns; nesting no deeper than three levels; functions around 60 lines.
- Wrap every error with context: `fmt.Errorf("creating sandbox for attempt %s: %w", id, err)`.
- Interfaces only at real boundaries (sandbox, model provider, blob store, GitHub client). Not one per struct.
- Table-driven tests; fakes over mocks; one behaviour per case.
- Each package has a `doc.go` stating its single responsibility.

Complexity limits are enforced in CI by `golangci-lint` (`gocyclo`, `gocognit`, `funlen`, `nestif`). If a function trips them, restructure it rather than raising the limit.

## Testing

- Model calls in tests use the fake replay provider in `internal/llm/faketest`. Never call a real provider from a test.
- Workflows are tested with Temporal's `testsuite`, without a running server.
- Integration tests use `testcontainers-go` for Postgres and Docker.
- Security controls have tests that attempt the attack: path escapes, injected instructions, canary leakage. These live beside the code they test.

## Review list

Tickets reference items in [`docs/review-list.md`](docs/review-list.md) (`RL-*`). When a ticket names one, explain in the PR description how the implementation satisfies it. These are the areas where plausible-looking code is most often wrong.
