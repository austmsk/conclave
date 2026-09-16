# Conclave — Architecture

Single-owner platform that takes GitHub issues on Austin's repositories, understands the existing code, plans the change, produces and tests implementations in isolated sandboxes, selects and integrates the best result, and opens pull requests for human review.

Companion documents: [`docs/scope-and-requirements.md`](docs/scope-and-requirements.md) (requirement IDs `FR-*`, `NFR-*`), [`docs/security-and-threat-model.md`](docs/security-and-threat-model.md), [`docs/agent-roles.md`](docs/agent-roles.md), [`docs/review-list.md`](docs/review-list.md) (`RL-*`).

## Shape of the System

A feature request flows through six stages, supported by five shared services.

```
                                    ┌─────────────────────────┐
  Repo analysis (codebase brief)    │ Orchestrator (Temporal) │
           ↓                        │ LLM router              │
  Planning (task graph)             │ Sandbox pool            │
           ↓                        │ Context store (Postgres)│
  Implementation (attempts)         │ Observability (OTel)    │
           ↓                        └─────────────────────────┘
  Evaluation (gates, then judge)
           ↓
  Integration (merge queue)
           ↓
  Pull requests (stacked, human-merged)
```

Milestone 1 implements a thin vertical slice: one issue, one implementer, one sandbox, one PR.

## Load-Bearing Decisions

**Deterministic code orchestrates; agents do bounded work.** There is no manager agent. Temporal workflow code decides what runs next, the way a program calls functions. An agent never sees the whole feature or controls what happens after it. This follows from Temporal's determinism requirement, from blast-radius containment (`RL-18`), and from cost and debuggability.

**Go everywhere.** Server, workers, and CLI. Temporal, Docker, and Ollama are themselves Go; single static binaries make multi-machine deployment a file copy. Cost: most agent tooling is Python, so the agent loop is written by hand — which is better anyway, since it is the part worth owning.

**Temporal owns execution state; Postgres owns domain records.** Anything queried or reported on lives in Postgres, because closed workflow histories are eventually deleted (`RL-14`).

**Pull-based workers.** Workers poll task queues, so adding a machine is starting a binary. Central services (Temporal, Postgres, blob store, router) run on one always-on machine; everything else scales out (`NFR-SC1`).

**One scheduler, one setting.** Sequential execution is parallel execution with a limit of 1. A deterministic function chooses the limit per run after planning; the owner can override (`FR-S13`, `RL-15`).

**Agents propose, platform code acts.** Pushing branches, opening PRs, and installing dependencies are platform actions performed after checks pass. No agent holds a credential or a network path (`FR-SEC5`).

**Deny by default.** No sensitivity tier, no run. No role in a repository's allowlist, no access. Tools absent from a role's list are denied (`FR-SEC12`, `FR-SEC32`, `FR-SEC3`).

## Technology

| Layer | Choice | Role |
|---|---|---|
| Language | Go | Everything |
| Orchestration | Temporal (Go SDK) | Durable workflows, retries, signals |
| Database | PostgreSQL + pgvector | Domain records, audit log, embeddings |
| DB access | `pgx` + `sqlc` + `goose` | Typed queries from visible SQL; migrations |
| HTTP | `net/http` | Webhook receiver; no framework |
| Logging | `log/slog` | Structured logs |
| Artifacts | Local disk → Cloudflare R2 | Transcripts, diffs, logs |
| Sandboxes | Docker (local) → Daytona (remote) | One `Sandbox` interface |
| Models | Official Anthropic / OpenAI / Google Go SDKs, Ollama | Behind a role-based router |
| Code intelligence | ripgrep, language servers (LSP) | Search and symbol queries |
| GitHub | GitHub App + `go-github` + `ghinstallation` | Events and actions |
| Networking | Docker Compose, Tailscale, Cloudflare Tunnel | Local stack, private mesh, public webhook |
| Observability | OpenTelemetry + Jaeger, Temporal UI | Traces and run inspection |
| CI | GitHub Actions | `go vet`, `golangci-lint`, `go test -race`, `workflowcheck`, `govulncheck` |

Deferred: tree-sitter (cgo complicates static binaries; language servers are more accurate). Rejected: MCP for agent tools (see threat model).

## Model Routing

| Role | Model | Notes |
|---|---|---|
| planner | `claude-opus-5` | Fallback filtered by tier |
| implementer | pool: `claude-fable-5-1`, `gpt-5.6-sol`, `gemini-3.8-flash` | One provider per attempt (Milestone 3) |
| judge | outside-provider rule | Whichever provider is not in the pair being compared |
| brief | `gemini-3.8-flash` | Large context, low cost |
| test_writer, pr_writer, discussion | `claude-opus-5` | Reasoning and writing |
| utility | local via Ollama | Log summaries, commit messages |
| embeddings | Qwen3-Embedding, local, ≤1024 dims | Must fit pgvector's index limit (`RL-4`) |

Pin exact model IDs. Never use `latest` aliases — they invalidate benchmark comparability. New releases enter as evaluation candidates, never as direct swaps.

## Domain Model

Work: `repos → feature_requests → plans (versioned) → tasks (+dependencies) → attempts`.
Evidence: `gate_results`, `comparisons`, `decisions`.
Context: `codebase_briefs` (pinned to a commit), `messages`.
Accounting: `cost_events`, `state_transitions` (append-only, insert-only DB role).

State changes use compare-and-set (`UPDATE … WHERE state = expected`) against one transition table with an exhaustive test (`RL-13`).

### Feature request statuses

| Status | Waiting on | Commands | Moves to |
|---|---|---|---|
| `planning` | nothing | `/cancel`, `/ask` | `awaiting_approval`, `needs_attention`, `cancelled` |
| `awaiting_approval` | owner | `/approve`, `/revise`, `/cancel`, `/ask` | `executing`, `planning`, `cancelled` |
| `executing` | nothing | `/cancel`, `/status`, `/ask` | `delivered`, `needs_attention`, `cancelled` |
| `needs_attention` | owner | `/retry`, `/revise`, `/cancel`, `/ask` | `executing`, `planning`, `cancelled` |
| `delivered` | terminal | `/revise` starts a linked run | — |
| `cancelled` | terminal | — | — |

There is no automatic `failed` status: every problem routes to `needs_attention` with a `reason`, and the owner decides. The workflow ends at `delivered`; merge outcomes are recorded on PR records via webhooks (`RL-9`).

Milestone 1 task statuses: `running`, `done`, `needs_attention`, `cancelled`. Attempt statuses: `running`, `passed`, `failed`, `errored`. Cleanup is tracked by `sandbox_released_at`, not a status, so the reaper can find orphans regardless of how an attempt ended (`RL-3`).

## Workflows

One parent workflow per feature request; one child per task; one long-running integration workflow per repository (merge queue, Continue-As-New, `RL-10`). Workflow IDs derive from the issue number, so duplicate webhooks deduplicate for free.

Inside a task: start from the integration branch → write tests → run attempts in parallel → deterministic gates → judge survivors pairwise → submit winner to the merge queue.

Determinism rules: no `time.Now()`, no `math/rand`, no raw goroutines, no network in workflow code. Sort before ranging over maps. Computed policies (such as the parallelism limit) are decided in activities and recorded, never recalculated in workflow code (`RL-16`).

## Git and Pull Requests

Two pushed branch kinds: `agent/<issue>/integration` (accumulator, one commit per task, pushed after each task so work survives a crash) and `agent/<issue>/pr-<n>` (pointers into that history at task boundaries). Attempt branches stay in the worker workspace; losing attempts become blob-store artifacts.

Base SHA is pinned at approval. One rebase onto the default branch at finalization, then a full gate rerun. Stacks are capped at 4; each PR's base SHA is recorded so restacking after a merge uses `git rebase --onto`, which works with any merge method (`RL-21`). Force-push only to `agent/` branches, only with `--force-with-lease`.

## Security Posture

Assume any agent can be manipulated; limit what a manipulated agent reaches. The worst realistic outcome of a hijacked agent is a rejected PR and wasted spend.

Key controls: sensitivity tiers enforced at the router (`RL-17`); sandboxes with no credentials, no network during the agent phase, no Docker socket (`RL-5`); per-role tool allowlists (`RL-11`); artifact frozen and hashed before gating, push refuses hash mismatches (`FR-SEC27`); execution-surface flagging including AI configuration files; provenance labels with nonce-delimited quarantine (`FR-SEC28`); GitHub App without `workflows` permission (`RL-7`).

## Milestones

| # | Goal | Done when |
|---|---|---|
| 1 | Walking skeleton | Labeled issue on Election Tally produces a CI-passing PR in ≥7/10 trials; zero orphaned sandboxes over 10 runs including 2 cancellations |
| 2 | Evaluation harness | Test writer and hidden tests in place; ≥20 replay tasks plus a SWE-bench subset; report of resolve rate, cost, time; flaky tests detected |
| 3 | Competition and judging | Best-of-N with pairwise judging measured against the single-agent baseline; ≥3 providers routed by role |
| 4 | Planning and integration | Multi-task feature planned, executed sequentially, integrated, delivered as stacked PRs; workers on ≥2 machines |
| 5 (stretch) | Parallel tasks | Same features run always-sequential, always-parallel, and automatic; default chosen from data |

## Repository Layout

```
cmd/server/          Webhook receiver and HTTP API
cmd/worker/          Temporal worker; flags select task queues
internal/contracts/  Shared types and interfaces (reviewed by hand)
internal/workflow/   Deterministic workflows
internal/activity/   Side effects: LLM, sandbox, git, GitHub
internal/agent/      Agent loop and tools
internal/llm/        Role-based router and provider adapters
internal/sandbox/    Sandbox interface; docker/ and daytona/
internal/gate/       Deterministic gates
internal/github/     App auth, webhooks, PR operations
internal/store/      Postgres access (sqlc-generated)
internal/repoindex/  Code intelligence (Milestone 3)
internal/eval/       Benchmark runner (Milestone 2)
docs/                Requirements, threat model, roles, review list, tickets, decisions
deploy/              Compose files, image definitions
```
