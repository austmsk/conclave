# Ticket 8-pre — Workflow prerequisites: build plan

Oct 26, 2026 · written before ticket 8 from a gap review of the merged code (tickets 1–7, 8a).

## TL;DR
- **Why this ticket exists.** Ticket 8 was already the riskiest ticket in Milestone 1. A review of the merged code found that the workflow has nothing to call for five things it needs on its first line: creating work rows, finding the repository row, releasing its own sandbox, knowing which image and limits to use, and telling the agent what the repository is. Pulling those out keeps ticket 8 to the workflow, the activities and the worker wiring.
- **No Temporal.** Nothing here imports the Temporal SDK. Everything is testable with the existing Postgres and Docker test harnesses.
- **One contract batch, reviewed by hand:**
  - `SandboxProvider.Release(ctx, id string) error`;
  - `SandboxProvider.CreateWithInstall(ctx, spec, install) (Sandbox, []ExecResult, error)`, lifting the Docker provider's existing method onto the interface;
  - `contracts.ReasonCancelled`;
  - `contracts.ReasonBudgetCap` (proposed; see Owner decisions).
- **What gets built:**
  - `internal/store/work.go`: create and update functions for repos, feature requests, plans, tasks and attempts, each idempotent on its natural key.
  - `internal/sandbox/docker`: `Release`.
  - `internal/sandbox/config.go` and `deploy/sandboxes.yaml`: per-repository image digest, limits, install commands, environment, default branch.
  - `deploy/toolchains.yaml` gains a `profile` block per repository, loaded by `agent.LoadToolchains`.
  - Actor-string helpers for the audit trigger.
- **Not built here:** the workflow, activities, `cmd/worker` wiring, the Temporal starter in `cmd/server`, the plan comment. Those are ticket 8, and the second half of this document records the decisions they must follow.

## Context
Milestone 1 is one issue, one implementer, one sandbox, one pull request (`milestone-1.md`). The requirement IDs this ticket serves are NFR-R2 (idempotent actions), NFR-R3 (guaranteed cleanup), FR-SEC12 (no tier, no run), FR-SEC30 (images pinned by digest) and NFR-M7 (plain, validated configuration). Review-list items: RL-2, RL-3, RL-13, RL-14.

### Gaps in merged code that this ticket closes
1. **No inserts for the work tables.** `internal/store` has `Transition` and `ApprovePlan` and nothing that creates a repo, feature request, plan, task or attempt. `llm.Router.Scope` refuses to run until those rows exist (`scope.go`, the `Attributed` check).
2. **No lookup of a repository by full name.** `RepoDirectory.Repo` takes the UUID. The webhook hands over a GitHub numeric ID and a full name (`github.RepoRef`), and the repos table stores neither a GitHub ID nor a seed.
3. **No way to release a sandbox by ID.** `contracts.SandboxProvider` has `Create` and `Reap`; `Close` needs the live object. The schema tracks cleanup per attempt (`attempts.sandbox_id`, `sandbox_released_at`, the unreleased partial index), but nothing writes either column. After a worker crash the workflow's deferred cleanup cannot name its container, and "zero orphaned sandboxes" cannot be checked from the database.
4. **No configuration for the image digest, resource limits, install phase or sandbox environment.** `docker.Provider.Create` requires all of them and refuses a tag. The java21 image exists only on the owner's machine.
5. **No source for `contracts.RepoProfile`.** The agent prompt renders language, layout hint and conventions, and nothing supplies them.
6. **No actor convention.** The audit trigger requires `conclave.actor` on every transition, and no code or document says what goes in it.
7. **Attempts cannot be cancelled.** Attempt states are `running`, `passed`, `failed`, `errored`, and `FailureReason` has no `cancelled` value, so a `/cancel` during the agent phase has no honest row to write.

### Owner decisions (2026-10-26)
- **Contract batch approved for hand review:** `Release` by ID on `SandboxProvider`, `CreateWithInstall` on `SandboxProvider` (approved later the same day so the install phase is configuration for Milestone 2's SWE-bench tasks, answering the ticket 3 question), and `ReasonCancelled` as a failure reason rather than a fifth attempt state. The ticket session shows the exact `internal/contracts` diff before committing it.
- **Proposed addition to the batch, to confirm at ticket start:** `ReasonBudgetCap`. When the router refuses an attempt's call because the feature request or the month is over its cap, the attempt did not fail on its own, and none of the existing reasons describe it. Without this value the attempt row would say `provider_error`, which is false. If declined, ticket 8 records `errored` with reason `provider_error` and the cap kind in the feature request's reason only.
- **Analyze step is owner-written issue sections.** The plan is parsed from the issue body (Task, Acceptance criteria, Hints), only when the owner authored the issue. No model call before `/approve`. See "Carried into ticket 8".
- **Ticket 8 posts the plan comment**, so `/approve` is never blind. The in-place status comment stays in 8b.
- **8-pre is its own ticket and session.**
- **Sandbox configuration is a new file**, `deploy/sandboxes.yaml`, loaded by `internal/sandbox`. The repo profile goes into `deploy/toolchains.yaml`, because both are what the agent is handed about a repository and `agent.LoadToolchains` already keys on it.

## Scope

### 1. Contract batch (`internal/contracts`)
```go
// Release removes the sandbox with the given ID and anything created for
// it, such as its network. It is idempotent: an ID that names nothing
// returns nil. It refuses an ID that names something the provider did not
// create. The workflow calls it from a disconnected context after
// cancellation, and the reaper calls it for attempts whose
// sandbox_released_at is still null (RL-3).
Release(ctx context.Context, id string) error
```
and, with the Docker provider's current signature and doc comment moved up unchanged:
```go
CreateWithInstall(ctx context.Context, spec SandboxSpec, install []ExecRequest) (Sandbox, []ExecResult, error)
```
both on `SandboxProvider`, and in `attempt.go`:
```go
ReasonCancelled FailureReason = "cancelled"  // the owner cancelled mid-attempt
ReasonBudgetCap FailureReason = "budget_cap" // a platform cap refused the call (proposed)
```
No other contract change. `Attempt`, `SandboxSpec` and the state tables are untouched.

### 2. Docker `Release` (`internal/sandbox/docker`)
- Looks the container up by ID. Removes it with force, then removes its per-sandbox network, in that order, the same way `Close` does; share the code.
- A container that lacks the `conclave.sandbox` label is refused with a typed error. `Release` must never remove something the provider did not create.
- Not found, for the container or the network, is success.
- Tests, in the existing Docker harness: create then `Release` by ID leaves no container and no network; `Release` twice returns nil both times; `Release` of a container created without the label is refused and the container survives; `Close` after `Release` still returns nil.

### 3. Work-table store functions (`internal/store/work.go`)
Every function takes a `DBTX` or a `pgx.Tx` like the existing code, checks UUIDs with `checkUUID`, and is idempotent on the table's natural key. "Idempotent" here means: a second call with the same key returns the existing row and reports `created == false`; it does not error, and it does not overwrite. A caller that needs to know a field changed compares the returned row.

| Function | Natural key | Notes |
|---|---|---|
| `EnsureRepo(ctx, db, contracts.Repo) (contracts.Repo, error)` | `full_name` | Upsert. Configuration is the authority for `tier` and `default_branch` (FR-SEC12), so an existing row is updated to match. Called at worker startup for every `repos.yaml` entry. |
| `(*RepoDirectory) RepoByFullName(ctx, fullName) (contracts.Repo, bool, error)` | `full_name` | Mirrors `Repo`. |
| `CreateFeatureRequest(ctx, db, FeatureRequestParams) (contracts.FeatureRequest, bool, error)` | `(repo_id, issue_number)` | State `planning`. Params: RepoID, IssueNumber, Title, WorkflowID. If the existing row's `workflow_id` differs, return `ErrWorkflowMismatch`: the same issue cannot belong to two workflows. |
| `CreatePlan(ctx, db, PlanParams) (contracts.Plan, bool, error)` | `(feature_request_id, version)` | State `proposed`. Body is `[]byte` of JSON, stored as `jsonb`. |
| `CreateTask(ctx, db, planID, contracts.TaskSpec) (contracts.Task, bool, error)` | `(plan_id, key)` | State `running`. |
| `CreateAttempt(ctx, db, AttemptParams) (contracts.Attempt, bool, error)` | `(task_id, ordinal)` | State `running`. Provenance columns are NOT NULL, so params carry role, prompt version, model and provider from the router configuration; the actual model is written back in `FinishAttempt`. |
| `SetAttemptSandbox(ctx, db, attemptID, sandboxID) error` | — | Write-ahead intent: set only when null or already equal; a different non-null value is `ErrSandboxMismatch`. Called before the sandbox is created with the ID the provider will use, or immediately after creation if the provider assigns it. See the decision below. |
| `MarkSandboxReleased(ctx, db, attemptID) error` | — | Sets `sandbox_released_at = now()` when null; a second call is a no-op. |
| `SetArtifactHash(ctx, db, attemptID, hash) error` | — | `UPDATE … WHERE artifact_hash IS NULL OR artifact_hash = $2`. Zero rows with a different stored value is `ErrArtifactMismatch`. The write-once trigger from 00007 stays as the last line of defence. |
| `FinishAttempt(ctx, tx, FinishParams) error` | — | One transaction: writes usage, provenance, report and `finished_at`, then calls `Transition` to the terminal state with the reason. Retrying after a success returns `ErrAlreadyApplied` from `Transition` and leaves the columns alone. |
| `UnreleasedAttempts(ctx, db, olderThan) ([]contracts.Attempt, error)` | — | Rows with `sandbox_id` set and `sandbox_released_at` null, older than the cutoff; uses the existing partial index. Ticket 10's reaper joins this against `Release`. |

**Sandbox ID timing.** `docker.Provider.Create` names the container itself. Ticket 8's attempt activity therefore calls `Create`, then `SetAttemptSandbox` as its first database write, before any agent step. The window between the two is covered by `Reap` by age, which already exists. Recording the ID before creation would need the provider to accept a caller-chosen name, which is a wider change than Milestone 1 needs.

**Tests** (Postgres through the existing `main_test.go` harness): one table-driven test per function with the idempotent second call as its own case; `ErrInvalidID` on a malformed UUID; `ErrWorkflowMismatch`, `ErrSandboxMismatch`, `ErrArtifactMismatch`; `EnsureRepo` updating a changed tier; `FinishAttempt` run twice leaves one audit row and one set of usage values; `UnreleasedAttempts` returns only rows past the cutoff. The `conclave_app` role already holds INSERT and UPDATE on these tables (00004:33); add a bypass-style test that it still cannot DELETE from any of them.

### 4. Sandbox configuration (`deploy/sandboxes.yaml`, `internal/sandbox/config.go`)
```yaml
repositories:
  owner/repo:
    default_branch: main
    image: ghcr.io/owner/conclave-sandbox-java21@sha256:0000000000000000000000000000000000000000000000000000000000000000
    limits:
      memory: 2GiB
      cpu_shares: 1024
      disk: 1GiB
      max_procs: 256
      timeout: 25m          # the whole sandbox; longer than the agent's wall clock
    command_timeout: 5m     # each gate command (gate.Commands.Timeout)
    install: []             # argv list run with registry network, or empty for no install phase
    env:                    # allowlisted names only; values for external services are canaries
      JAVA_TOOL_OPTIONS: "-Duser.timezone=UTC"
```
- `sandbox.LoadConfig(io.Reader) (map[string]RepoSandbox, error)` with `KnownFields(true)`, the same shape of loader and placeholder behaviour as `agent.LoadToolchains`. `RepoSandbox` holds `DefaultBranch`, `Image`, `Limits contracts.ResourceLimits`, `CommandTimeout`, `Install [][]string`, `Env map[string]string`.
- Validation reuses the Docker provider's rules so startup fails where `Create` would: image pinned by digest (`docker.validateSpec` regex; export it or move the regex to `sandbox`), every limit greater than zero, env keys matching the provider's `envKey` and none of its reserved names, `default_branch` non-empty, `timeout` greater than `command_timeout`.
- The shipped file keeps the `owner/repo` placeholder and an all-zero digest, which fail validation, like the other deploy files. The real entry lives in git-ignored `deploy/local/sandboxes.yaml`.
- A helper `config.SameRepositories(name string, sets ...map[string]struct{}) error` is not worth a package. Instead `sandbox.LoadConfig` takes no cross-file knowledge, and ticket 8's `cmd/worker` checks that `repos.yaml`, `toolchains.yaml`, `gates.yaml` and `sandboxes.yaml` name exactly the same repositories, failing startup with the differing names. Write that check as a small function in `cmd/worker` with a test, in ticket 8.
- **Image.** For the Milestone 1 trials the java21 image is built locally and its local digest goes in `deploy/local/sandboxes.yaml`. The CI build and GHCR publication from FR-SEC30 are recorded as deferred to ticket 10 or later, in the decision record this ticket writes.

### 5. Repo profile (`deploy/toolchains.yaml`, `internal/agent/toolchains.go`)
```yaml
repositories:
  owner/repo:
    test: [...]
    build: [...]
    lint: [...]
    profile:
      language: Java
      layout_hint: "Sources in src/, tests in test/, JUnit console launcher under lib/"
      conventions: "JUnit 5, one test class per production class, no external build tool"
```
- `LoadToolchains` returns a struct per repository holding `Commands` and `contracts.RepoProfile`; `FullName` is filled from the key. All three profile fields are required and non-empty.
- Existing tests gain the profile; the placeholder still fails.

### 6. Actor helpers (`internal/store/actor.go`)
```go
func ActorWorkflow(workflowID string) string // "workflow:" + id, for platform transitions
func ActorGitHub(userID int64) string         // "github:" + id, for owner commands
const ActorWorker = "worker"                   // startup seeding and the reaper
```
One doc comment explains that the audit trigger copies the value into `state_transitions.actor` and that it is read by people, so the prefix is the only structure.

### 7. Documents
- Decision record: sandbox release by ID and write-ahead sandbox IDs; why a reason rather than a cancelled state; image digest from local build for Milestone 1.
- Guide page `docs/guide/tickets/08-pre-workflow-prerequisites.md` from `_template.md`.
- `ARCHITECTURE.md` repository layout: no change. `docs/tickets/milestone-1.md`: this ticket's row and section are already added.

## Done when
- [ ] The `internal/contracts` diff is exactly the batch above and was shown to the owner before commit.
- [ ] `CreateWithInstall` is reachable through `contracts.SandboxProvider`; the existing install-phase attack tests still pass unchanged.
- [ ] `Release` by ID removes the container and its network; it is idempotent; it refuses an unlabelled container; the Docker tests prove each.
- [ ] Every work-table function has an idempotency test that runs it twice and asserts one row and one audit entry.
- [ ] `EnsureRepo` makes the database tier match configuration, and `llm.Router.Scope` succeeds for a feature request and attempt created through these functions (one end-to-end store test).
- [ ] `SetArtifactHash` with a different value fails with `ErrArtifactMismatch` and the trigger still refuses a direct UPDATE.
- [ ] `conclave_app` cannot DELETE from any work table.
- [ ] `sandbox.LoadConfig` rejects the placeholder, an image by tag, a zero limit, a reserved env name, and a sandbox timeout shorter than the command timeout, each with a clear message; it accepts a valid file.
- [ ] `agent.LoadToolchains` requires the profile and returns it.
- [ ] `UnreleasedAttempts` returns the deliberately unreleased row and not the released one.
- [ ] Guide page and decision record written. `make check` passes.

**Requirements:** NFR-R2, NFR-R3, FR-SEC12, FR-SEC30, NFR-M7 · **Review list:** RL-2, RL-3, RL-13, RL-14

---

## Carried into ticket 8
These decisions were made during the same review and bind the ticket 8 session. They override `Ticket 8 Split — Conclave.md` where they differ. The ticket 8 session reads this section before planning.

### Analyze step (owner-scoped)
- The plan is parsed from the issue body, with no model call. Three markdown sections are recognised by heading, case-insensitively: `Task` (becomes `TaskSpec.Description`), `Acceptance criteria` (each list item becomes one `AcceptanceCriteria`, IDs `AC1…`), `Hints` (appended to the description under its own label). The task key is `T1`. A missing Task or an empty criteria list moves the feature request to `needs_attention` with reason `error` and a detail saying which section is missing.
- Sections are parsed only when the issue author is the owner (`IssueSnapshot.Author`, numeric ID). For any other author the whole body is quarantined as `IssueText` and the plan is not derived from it; the feature request goes to `needs_attention` with reason `error` and a detail asking the owner to write the task. Milestone 1 trials are owner-written issues.
- `plans.body` is the frozen agent input: the `TaskSpec`, the quarantined `IssueText`, the `RepoProfile`, and the `Constraints` built from `gates.yaml`. The implement activity reads the plan body, not the issue, so what was approved is what runs (FR-SEC28).
- The plan comment is posted at `awaiting_approval` by an activity that is idempotent by a marker in the comment body (search the issue's comments by the App for the marker before creating). Ticket 8b's status comment is a separate comment.
- A `RepoProfile` comes from `toolchains.yaml`; `Constraints` from `gates.yaml` (`max_changed_lines`, protected paths, execution-surface paths; `AllowDependencyAdditions` false).

### Activity shape, timeouts, retries
- **One attempt is one activity** on one worker: create sandbox (with install if configured), set the sandbox ID on the attempt, run the agent, freeze, gate, record the outcome, release the sandbox. The merged runner owns the loop and the gate needs the live sandbox and the host tree, so this is decided by the code. Ticket 4's "one model call per activity" is superseded; the decision record for ticket 8 says so.
- The activity heartbeats from a goroutine every 30 seconds. Cancellation reaches the agent through the activity context, which the runner already honours. A heartbeat timeout of 2 minutes.
- `StartToCloseTimeout` is the explicit sum: sandbox create and install 3 min, agent wall clock 20 min (`Implementer.MaxWallClock`, which also bounds the 10-minute adapter timeout), freeze and gates 3 × `command_timeout` plus 2 min staging, margin 5 min. With the sample config that is 45 min. The decision record states the sum.
- **The attempt activity has `MaximumAttempts: 1`.** Attempts cannot restart and `artifact_hash` is write-once, so a Temporal retry would double-run or fail. If the activity errors, the workflow records the attempt `errored` (if the activity got far enough to create the row) through a separate small, retryable, idempotent activity, then starts a new ordinal. Small activities (database writes, comments) use the default retry policy and are idempotent.
- The sandbox is released in a deferred `workflow.NewDisconnectedContext` activity that calls `Release` with the recorded sandbox ID and then `MarkSandboxReleased`. It runs on every exit path including cancellation. If no sandbox ID was recorded, it is a no-op.

### Outcome table
Every row has a workflow test. Reason values are `contracts.FailureReason` for attempts and the ticket 8 reasons for feature requests.

| Outcome | Attempt | Task | Feature request |
|---|---|---|---|
| All gates pass | `passed` | `done` | stays `executing`, then deliver |
| Any gate fails | `failed`, `gate_failed` | `needs_attention` | `needs_attention`, `attempt_failed` |
| Runner limit: step, token, wall clock, repeat errors, invalid output | `failed`, that reason | `needs_attention` | `needs_attention`, `attempt_failed` |
| Sandbox, provider or gate-environment error, first time | `errored`, `sandbox_error` or `provider_error` | stays `running` | stays `executing`; new ordinal |
| Same, second time | `errored` | `needs_attention` | `needs_attention`, `error` |
| Feature-request or monthly cap refused the call | `errored`, `budget_cap` (or `provider_error` if the addition is declined) | `needs_attention` | `needs_attention`, `budget_cap:<kind>` |
| `/cancel` during the attempt | `errored`, `cancelled` | `cancelled` | `cancelled` |
| `/cancel` in `awaiting_approval` or `needs_attention` | no attempt | `cancelled` if one exists | `cancelled` |
| Non-retryable error outside an attempt | no attempt | unchanged | `needs_attention`, `error` |

### Signals
- Signal handlers check the workflow's in-memory status at the moment they run. `/approve` is accepted only in `awaiting_approval`; anything else is dropped with a log line (8b adds the reply). This is deterministic because handlers and the main flow run in the same workflow goroutine scheduler.
- Each handler keeps a bounded set of applied `CommentID`s and ignores a redelivery.
- Plan-ID binding is not needed while a feature request has one plan version; revisit with `/revise` in Milestone 4.

### Deliver stub
The ticket 8 deliver activity does not mark `delivered`. It moves the feature request to `needs_attention` with reason `error` and detail `delivery arrives in ticket 9`, and records the artifact hash and base SHA it would have pushed. Its signature takes `(attemptID, artifactHash, baseSHA)` so ticket 9 fills the body without changing the call.

### Workflow ID reuse
`WorkflowIDReusePolicy` is `RejectDuplicate`. Relabelling a delivered or cancelled issue in Milestone 1 is refused by Temporal and logged by the starter. The unique index on `(repo_id, issue_number)` agrees with this.

### Milestone 2 seams
`docs/tickets/m1-exit-checklist.md` lists what Milestone 2 must find in place. For ticket 8 that means:
- **The task is a child workflow**, `TaskWorkflow`, per `ARCHITECTURE.md`. Its input is task ID, attempt ordinal, base SHA and repo full name, all serializable; it makes no GitHub call and does no delivery. `FeatureWorkflow` starts it with `ParentClosePolicy` terminate so `/cancel` propagates, and the deferred release runs inside the child, where the sandbox ID lives. Milestone 2's benchmark starts `TaskWorkflow` directly. A test cancels the parent and asserts the child's sandbox is released.
- **Config digests in the plan body.** The analyze activity records the SHA-256 of each loaded deploy file alongside the frozen agent input, so every attempt's configuration is recoverable from the plan it ran under.
- **The attempt activity takes the role as input** and does not assume `agent.Implementer`; Milestone 2 passes `test_writer`.
- **Install through the interface.** The attempt activity calls `CreateWithInstall` when `sandboxes.yaml` lists install commands and `Create` otherwise, through `contracts.SandboxProvider` only.

### Other conventions
- Diffs, transcripts and tool output never enter workflow history: activities pass blob keys and hashes, never bytes. A test plants a canary string in a fake agent's transcript and asserts it is absent from the test suite's recorded history.
- The completion report is stored in `attempts.report` through `FinishAttempt`; ticket 9's PR writer reads it from there.
- "Traceable end to end" is the join `feature_requests.workflow_id → plans → tasks → attempts → gate_results, cost_events, tool_calls, state_transitions`; one store test walks it after a fake run.
- The worker runs one attempt activity at a time (`MaxConcurrentActivityExecutionSize` on the attempt task queue) for the trials. Two issues labelled at once queue.
- Workflow versioning: drain in-flight workflows before deploying a workflow change in Milestone 1; from the first change in 8b, use `workflow.GetVersion`.
