# 0012 — The Temporal Go SDK, one activity per attempt, and the attempt timeout

**Date:** 2026-10-28
**Status:** proposed

## Context

Ticket 8 is the first code that needs a workflow engine. `ARCHITECTURE.md` names Temporal, and the owner approved the Go SDK in the 8-pre session. Three choices had to be made with it:

- **Which dependency, pinned how.** NFR-M4 asks for a record for every new dependency.
- **How finely to split an attempt into activities.** Ticket 4 recommended one model call per activity, with transcripts kept out of workflow history. That predates the agent runner (ticket 5) and the gate (ticket 7).
- **What `StartToCloseTimeout` the attempt gets.** It must sit above the router's 10-minute adapter timeout, cover the agent's wall clock and the gate commands, and stay put for a run already in flight (RL-16).

## Decision

- **`go.temporal.io/sdk` v1.50.0, pinned exactly**, with `workflowcheck` v0.5.0 in `make check`. `go.temporal.io/api` becomes a direct requirement because the workflow and the starter use its enums, error types and JSON history format. Both modules were already in the module graph through the SDK. Tests use the SDK's `testsuite` and `worker.WorkflowReplayer`; no mocking library is added (the workflow tests register stateful fakes by activity name instead of `OnActivity`).
- **One attempt is one activity, `RunAttempt`.** It prepares the tree at the pinned base through `TreeSource`, creates the sandbox (through `CreateWithInstall` when `sandboxes.yaml` lists install commands), records the sandbox ID, runs the agent, freezes and records the artifact hash, gates it, records the outcome and releases the sandbox, all on one worker. The runner owns the model loop, and the gate needs the live sandbox and the host clone, so the attempt cannot be split across workers without moving the sandbox. This supersedes ticket 4's "one model call per activity". Transcripts, diffs and tool output still stay out of history: activities pass IDs, hashes and reasons only (RL-14), and `TestCanaryNotInHistory` checks it.
- **`RunAttempt` is never retried by Temporal** (`MaximumAttempts: 1`). An attempt cannot restart: `artifact_hash` is write-once and a second run would repeat the agent's spend. When it returns an error, the small `SettleAttempt` activity records the attempt `errored` if it is still running and returns what Postgres holds, and the task workflow routes on that record, starting a new ordinal once. Every other activity uses Temporal's default retry policy, bounded by a one-hour `ScheduleToCloseTimeout` so an outage ends in `needs_attention` rather than an endless retry. Each is idempotent.
- **The attempt timeout is an explicit sum**, computed by `activity.AttemptTimeout` from the role spec and the repository's `command_timeout`, and recorded in history by `PrepareAttempt` so a configuration change cannot alter it mid-run:

  | Part | Shipped value |
  |---|---|
  | Clone, sandbox create and install (`SandboxCreateBudget`) | 3 min |
  | Agent wall clock (`Implementer.MaxWallClock`, which also bounds the 10-minute adapter timeout) | 20 min |
  | Build, lint and test gates (3 × `command_timeout`) | 15 min |
  | Freezing and staging (`StagingBudget`) | 2 min |
  | Margin for recording and release (`AttemptMargin`) | 5 min |
  | **Total** | **45 min** |

- **Heartbeats come from a goroutine** inside `RunAttempt` every 30 seconds, with a 2-minute `HeartbeatTimeout`. The router's rate limiter has no heartbeat hook, and neither do the runner or the gate, so the beat cannot come from the work itself. Temporal delivers a cancellation only through a heartbeat, and the runner honours the activity context.
- **A task names its repository by full name and its role; the worker supplies the rest.** `TaskInput` and `RunAttemptInput` carry no GitHub reference. The attempt reads the repository's numeric ID from the plan body and gets its tree from `activity.TreeSource` (`GitHubTrees` in Milestone 1, cloning through the App's one installation), and looks its role spec up in `activity.Deps.Roles`, which the worker fills. Milestone 2's benchmark runs `TaskWorkflow` with its own tree source and adds `test_writer` to the map, with no change to the activities.
- **The attempt queue is paired with the workflow's queue** (`<queue>-attempt`) and polled with one slot, so attempts run one at a time and a test or benchmark worker on its own queue never takes another worker's attempts.

## Consequences

- A worker that dies mid-attempt costs that attempt: the heartbeat timeout fails the activity, `SettleAttempt` records it `errored/sandbox_error`, and a new ordinal runs. Its sandbox is released by the task workflow's deferred release, or by the worker's startup sweep (`SweepOrphans`) if the ID was recorded, or by `Reap` by age if the worker died between `Create` and `SetAttemptSandbox`.
- The sweep's cutoff (`-sweep-after`, two hours by default) must stay above the attempt timeout, or a second worker would remove a live attempt's sandbox. The worker refuses to start when it does not exceed `Deps.LongestAttempt`, the longest timeout over every configured role and repository.
- A change to the role's wall clock or a repository's `command_timeout` changes the timeout of attempts prepared after it, not of those in flight.
- Workflow changes are deployed by draining in-flight workflows in Milestone 1; from ticket 8b's first change, `workflow.GetVersion`. The replay fixtures under `internal/workflow/testdata/` are recaptured with `CONCLAVE_CAPTURE_REPLAY=1` when a deliberate change makes them diverge.
