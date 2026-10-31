# How Conclave works

Conclave takes a GitHub issue on one of the owner's repositories and turns it into a pull request for the owner to review and merge. It never merges anything itself.

The rule behind the whole design: **Go code decides; models only do bounded jobs.** No model chooses what happens next. A Temporal workflow, which is ordinary deterministic Go, calls agents the way a program calls functions. Each agent sees only its own job, in a sandbox with no credentials and no network. Everything that touches the outside world (pushing a branch, opening a PR, installing dependencies) is done by platform code after checks pass.

This page follows one request through Milestone 1, the walking skeleton: one issue, one implementer, one sandbox, one PR. Each step names the package that does it and whether it exists yet.

## The path of one request

```
 GitHub issue labeled
        │
        ▼
 1. Webhook received ─────────── internal/github, cmd/server      ticket 6
        │
        ▼
 2. Workflow started ─────────── internal/workflow                ticket 8
        │   (one per feature request; ID from the issue number)
        ▼
 3. Records written ──────────── internal/store                   ticket 2 ✓
        │   (feature request, plan, task, attempt; every state change audited)
        ▼
 4. Owner approves ───────────── /approve, base commit pinned     tickets 6, 8, 8b
        │
        ▼
 5. Tree cloned and cleaned ──── internal/sandbox                 ticket 3 ✓
        │
        ▼
 6. Sandbox created ──────────── internal/sandbox/docker          ticket 3 ✓
        │   (install with network, then network cut)
        ▼
 7. Agent works ──────────────── internal/agent                   ticket 5
        │   │
        │   └─ every model call ─ internal/llm (router)           ticket 4 ✓
        │                          budget caps                    ticket 8a
        ▼
 8. Diff frozen and gated ────── internal/gate                    ticket 7
        │
        ▼
 9. Branch pushed, PR opened ─── internal/github, activity        ticket 9
        │
        ▼
   delivered  (owner reviews and merges by hand)
```

✓ means merged. Tickets 1–4 are done, so the foundations exist: shared types, the database, the sandbox, and the model router. Nothing connects them yet. That is ticket 8's job, and `cmd/worker` only logs a line today.

### 1. Webhook received (ticket 6)

GitHub sends an event when an issue is labeled or a comment is posted. `cmd/server` checks the signature and acknowledges within two seconds. Only commands from the owner's numeric user ID are honoured. The App's own comments and other bots are ignored, so a model's output can't issue a command. Accepted work goes on a queue for the workflow starter, and the request returns without waiting on it. See [ticket 6](tickets/06-github-app-and-webhooks.md).

### 2. Workflow started (ticket 8)

A Temporal workflow is code whose progress is saved after every step. If a worker dies, another worker replays the saved history and carries on. For that to work, workflow code must make the same decisions every time it runs. So it never reads the clock, generates random numbers, touches the network, or ranges over a map unsorted (see `CLAUDE.md`). Anything with a side effect is an **activity** in `internal/activity`.

The workflow ID comes from the repository's numeric ID and the issue number (`github.FeatureWorkflowID`), so a duplicate webhook can't start a second run.

### 3. Records written (ticket 2, merged)

Temporal holds execution state. Postgres holds the records people query: repos, feature requests, plans, tasks, attempts, gate results, cost events, and an audit log of every state change. Each record kind has a fixed set of legal moves, defined in `internal/contracts/state.go`. Every move goes through `store.Transition`, which only updates a row if it is still in the state and revision the caller last read. A database trigger writes the audit row. See [ticket 2](tickets/02-contracts-and-schema.md).

The rows themselves are created by idempotent store functions (`EnsureRepo`, `CreateFeatureRequest`, `CreatePlan`, `CreateTask`, `CreateAttempt`): a retried create returns the existing row instead of adding a second one. See [ticket 8-pre](tickets/08-pre-workflow-prerequisites.md).

A feature request has six statuses: `planning`, `awaiting_approval`, `executing`, `needs_attention`, `delivered`, `cancelled`. There is no `failed`. Every problem, including a budget cap, moves it to `needs_attention` with a reason, and the owner decides what happens next (`/retry`, `/revise`, `/cancel`).

### 4. Owner approves (tickets 6, 8 and 8b)

The owner comments `/approve`. Approval pins the base commit (`base_sha`), and every task runs against that commit even if the default branch moves on.

Every owner command is decided against the status the feature request is in when it arrives, using one command table (`ARCHITECTURE.md`, `workflow.validCommands`). A command that doesn't fit the status gets a reply and changes nothing. `/status` and `/help` reply. `/retry` from `needs_attention` re-checks the budget caps, then continues in the phase the run left. A stop during planning therefore still waits for a fresh `/approve`. Each command is recorded, accepted or rejected, in the append-only `commands` table. One status comment on the issue is edited in place after every move, and it names the reason, any cap that was hit, and the commands available. A command for a run that has already ended (`delivered`, `cancelled`) is answered by a short workflow on the worker. See [ticket 8b](tickets/08b-commands-and-status-surface.md).

### 5–6. Tree prepared, sandbox created (ticket 3, merged)

The worker clones the repository into its own workspace. The installation token is passed only through environment variables for that one git command and is never written to disk. `sandbox.PrepareTree` then strips remotes, credential helpers and reflogs. The Docker provider copies the cleaned tree into a hardened container: non-root, no capabilities, read-only root, size-limited scratch space, no Docker socket, no bind mounts, and no inherited environment. Dependencies install with network access, then the network is cut before the agent gets control. See [ticket 3](tickets/03-docker-sandbox.md).

The image, limits, install commands and environment for each repository come from `deploy/sandboxes.yaml`, which is checked at startup. The attempt records its sandbox ID as soon as the sandbox exists, so cleanup can release it by ID with `SandboxProvider.Release` even after the worker restarts. See [ticket 8-pre](tickets/08-pre-workflow-prerequisites.md).

### 7. Agent works (ticket 5), calling models through the router (ticket 4, merged)

The agent loop in `internal/agent` sends the task to a model and runs the tool calls the model asks for (read a file, write a file, apply a patch, run the tests) inside the sandbox. It repeats until the model calls `finish` or hits a limit.

- **Tools.** The role's tool list decides which tools are offered. A call to any other tool is refused before the sandbox sees it.
- **Paths.** Every path is checked and refused, never cleaned up, and `.git` is always out of reach.
- **Processes.** No process outlives a sandbox command, so nothing can swap a path for a symlink while a file is read or written.
- **Audit.** Before a result goes back to the model, the worker stores the full output in the blob store and writes an audit row to `tool_calls`. The row records the tool, the full arguments, the exit code and the duration. If either write fails, the attempt ends.
- **Limits.** Steps, tokens, wall clock and repeated identical failures are enforced by code.
- **The report.** It is validated against its schema, with one chance to repair it.

See [ticket 5](tickets/05-agent-loop-and-tools.md).

Every model call goes through the router in `internal/llm`. Before any provider sees a prompt, the router checks the repository's **sensitivity tier**: which providers may see this repository's code at all. It also checks whether the role is allowed on the repository and whether the call fits the remaining **token budget**. Afterwards it records the cost under an idempotency key, so a retried call isn't billed twice. See [ticket 4](tickets/04-llm-router.md). Ticket 8a adds the per-feature-request and monthly caps.

### 8. Diff frozen and gated (ticket 7)

When the agent finishes, the diff is read twice and must come back identical. No process outlives the command that started it, so nothing can still be writing. The diff is then stored as an **artifact**: an envelope binding it to its base commit, named by its SHA-256.

The gates check that artifact, never the agent's live workspace.
- **Static gates**, over the diff alone:
  - diff size;
  - secrets (a pinned `gitleaks` on the worker);
  - protected paths such as CI configuration, which fail;
  - test weakening, such as deleted tests, removed test cases or new skips;
  - execution surface: files that make code run at install, build or commit time, or reconfigure AI tools (`package.json`, `.husky/`, `.gitattributes`, `.claude/`). These are flagged, not failed.
- **Command gates**, only if every static gate passes. The sandbox's workspace is wiped, refilled from the clean tree on the worker, and the recorded diff is applied and re-diffed to prove it reproduces. Then build, lint and tests run; the tests must leave a JUnit report showing tests actually ran.

A flagged artifact, or a sensitive repository, is gated in a fresh `clean_room` sandbox rather than the agent's. A broken environment makes the attempt `errored` and records nothing, so a retry starts clean. Every result is recorded with the artifact's hash, so the diff that gets pushed is provably the one that was checked. See [ticket 7](tickets/07-gates-and-artifact-integrity.md).

### 9. Pull request delivered (ticket 9)

Platform code pushes to an `agent/` branch and opens a draft PR as the App's bot identity. The push refuses any diff whose hash doesn't match a passing gate result. The PR is marked ready once the repository's own CI passes. The workflow ends at `delivered`. Merging is the owner's job.

## Cleanup and failure

- **Sandboxes are always released.** Cleanup runs in a deferred function on a disconnected context, so it runs even when the workflow is cancelled. `Reap` sweeps containers that a dead worker left behind.
- **Every activity is safe to run twice.** Temporal retries activities, and a retry may follow a side effect that worked but was never reported. Cost events, state transitions, and PR creation are each built so the second run has no extra effect.
- **Nothing fails open.** A repository with no tier, a role not on its allowlist, a model the tier forbids, or a missing config value is an error, never a quiet fallback to something more permissive.

## Where to go next

- [`ARCHITECTURE.md`](../../ARCHITECTURE.md): the decisions above, stated as decisions, plus later milestones (planning, competing attempts, judging, stacked PRs).
- [`docs/tickets/milestone-1.md`](../tickets/milestone-1.md): the spec for every Milestone 1 ticket and the order they run in.
- [`docs/security-and-threat-model.md`](../security-and-threat-model.md): what the controls defend against.
