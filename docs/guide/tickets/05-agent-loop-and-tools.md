# Ticket 5 — Agent loop and tools

**Status:** in review on branch `ticket/5` · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-5--agent-loop-and-tools) · **Plan:** [`ticket-5-plan.md`](../../tickets/ticket-5-plan.md) · **Requirements:** FR-SEC3, FR-SEC24, FR-SEC29 (FR-SEC28 on the runner side) · **Review list:** RL-6, RL-11

## In one paragraph

The part of Conclave that actually does the work. `internal/agent` runs one agent session:
- It sends the task and the role's tools to the model through the router.
- It runs each tool call the model makes inside the sandbox.
- It writes an audit row for every call before the model sees the result, and repeats until the model calls `finish` or a limit stops it.

There are ten tools: `read_file`, `list_dir`, `search`, `write_file`, `apply_patch`, `run_tests`, `run_build`, `run_lint`, `run_command`, and `finish`. Around them, the ticket added:
- a `tool_calls` audit table;
- a content-addressed blob store (`internal/blob`) for full tool output;
- an owner-edited `deploy/toolchains.yaml` holding each repository's test, build and lint commands;
- a change to the Docker sandbox: **no process survives a sandbox command**.

## Why it exists

An agent is the one component that reads untrusted text (the issue, the repository's files, command output) and can act on it. A manipulated agent can only misuse the tools its role was given, so the first defence is the tool list itself (`FR-SEC3`, `RL-11`).

The second defence is path handling:
- Every path a file tool touches is checked and *refused*, never cleaned up (`FR-SEC29`).
- `.git` internals and denylisted paths stay out of reach.
- A planted symlink must not let a read or write leave the workspace.

The third is the audit:
- The worker records every call: tool, full arguments, exit code, duration, and a pointer to the full output (`FR-SEC24`).
- It records each call before the result goes back to the model, so the agent can't edit its own trail.

The loop itself has to be correct in the ways `RL-6` warns about:
- every tool call in a reply is answered, in order;
- limits are enforced by code, never requested of the model;
- an identical failure repeated over and over ends the session instead of burning budget.

## How it works

**Wiring.** `agent.New(Config{...})` builds a runner for one attempt. The caller supplies:
- the router, already scoped: in ticket 8 that is `llm.Router.Scope` with the same `AttemptID`;
- a `Recorder`, which `store.ToolCalls` implements;
- a `blob.Store`;
- the attempt ID and a `SessionKey` that is unique per run;
- the repository's `Commands`, its `Denylist`, the output cap shown to the model, and the longest any one command may run.

`New` builds the **registry**: every tool this runner can provide. A configured command tool exists only if its command is configured.

**Starting a session: `Runner.Run`** (`runner.go`).
1. **Check the role spec.** All four limits must be positive. `Role` and `PromptVersion` must be set. Otherwise the result is `ErrInvalidConfig`, before any model call.
2. **Decide what is offered.**
   - The offer is the role's allowlist ∩ the registry, in allowlist order.
   - A `ReadOnly` role loses `write_file`, `apply_patch` and every `run_*` tool, whatever its list says.
   - A listed tool the registry can't provide is a configuration error, not a silent omission.
   - So is a role without `finish`.
3. **Build the first message** (`prompt.go`).
   - The system prompt is static and versioned (`Implementer.PromptVersion`, decision 0006).
   - The one user message holds the task, the repository profile, the previous attempt's failures on a retry, and the issue text. The issue text sits **unchanged** under a heading saying it is untrusted; quarantining it is the webhook side's job.
   - Tool output only ever reaches the model as `tool_result` blocks.
4. **Start the clock.** The session runs under `context.WithTimeoutCause(…, errWallClock)`. The runner can then tell its own wall clock (`wall_clock`) from the caller cancelling, which is an error.

**One turn** (`loop.go`: `turn → execCalls → execOne → answer → record`).
1. **Before the request:** if no tokens are left, the session ends with `token_limit`. Otherwise `MaxTokens` is set to what remains.
2. **The model call.** On a router error:
   - The *attempt* budget (`llm.BudgetError` of kind `attempt`) ends the session with `token_limit`.
   - The feature-request and monthly caps are the platform's problem, not the agent's. They come back as an error with no `Reason`, still matching `llm.ErrBudgetExceeded`, so ticket 8 can route them to `needs_attention`.
   - Anything else is `provider_error`.
3. **After the reply:** usage is added up, and a reply that pushes past `MaxTokens` ends the session before any of its calls run.
4. **A reply with no tool call** gets one "call finish" nudge. That uses the session's single repair.
5. **Each call, in order**, with every call ID answered:
   - **Step limit.** It is checked before each call: `MaxSteps = N` allows exactly N calls. Calls a stop leaves unrun are still answered, and recorded as `not executed: <reason>`.
   - **Not offered.** A call to a tool that wasn't offered is refused (`tool_not_offered`, `rejected = true`), counted as a step, and never reaches the sandbox.
   - **Run the tool.** Tools return their full output. On an error, the runner first checks its own wall clock, then `classify` (`refusal.go`) sorts the error:
     - a `*Refusal`, or an error wrapping a sandbox sentinel (`sandbox.ErrInvalidPath`, `ErrNotFile`, `ErrFileTooLarge`, `ErrOutputTooLarge`), is shown to the model so it can correct course;
     - anything else means a broken sandbox: `sandbox_error`.
   - **Store, truncate, record.** In that order, *before* the result goes back:
     - Non-empty output goes into the blob store under `<repoID>/<sha256>`.
     - The model sees at most `OutputCap` bytes: the head and tail around a marker, cut on UTF-8 boundaries, with `Truncated` and `ArtifactKey` set.
     - The audit row is written.
     - Both writes run detached from the session's context with a 10-second limit, so a call cut off by the wall clock is still recorded.
     - If either write fails, the attempt ends with an error (fail closed).
   - **Repeat errors.** A failure (an error, or a non-zero exit) is counted under a key of tool, canonicalised arguments, exit code, and a hash of the output and error. `MaxRepeatErrors` identical failures ends the session with `repeat_errors`.
6. **Finish.** `finish` must be the only call in its reply. If it isn't, it is refused, the other calls still run, and the repair is not used.
   - The report is checked by `validateReport` (`report.go`). One field table produces both the JSON Schema and the validator. Required fields, unknown fields at any depth, null arrays and trailing data are all checked. The error lists every problem, for the model's one repair.
   - The first invalid report gets the errors back, which uses the repair. A second gives `invalid_output`.

**Errors Run returns.** Once a session has started, every error is a `*SessionError`. Its message is written by the agent package (role, tool, step, budget kind), so a caller may store it; Temporal writes activity errors into workflow history. The raw router or sandbox error is reachable only through `errors.Is` and `errors.As`, because it may carry a credential. This holds even when recording the calls a failure left also fails: `abandon` wraps both in one `SessionError`.

The audit's `error` column holds sanitized codes only. The model sees refusal text: the agent package's own, or, for an error wrapping a sandbox sentinel, the backend's message about the agent's already-validated path. Any other tool error ends the session, and the model sees nothing of it.

**The tools.**
- **Paths** (`internal/sandbox/path.go`, `internal/agent/path.go`). `sandbox.CheckPath` refuses:
  - empty, absolute, trailing-slash and over-long paths;
  - `.`, `..`, dots-only components such as `....`, and `%XX` escapes;
  - backslash, NUL, C0 and C1 control characters, bidi overrides, and invalid UTF-8;
  - `.git` in any spelling a macOS or Windows checkout would treat as `.git`: any case, trailing dots or spaces, characters HFS+ ignores, `git~1`.

  `checkPath` then applies the repository's denylist, matched as runs of whole path components. `secrets` blocks `a/secrets/x`, not `secretsfoo`. Every refusal wraps `sandbox.ErrInvalidPath`.
- **`read_file`, `write_file`**: check the path, then `Sandbox.ReadFile` or `Sandbox.WriteFile`. `read_file` takes an optional 1-based line range.
- **`list_dir`**: a script walks each path component with `[ -L ]` (a symlink is refused), then lists entries. `.git` and denylisted entries are hidden in Go.
- **`search`**: after the same component walk, `find ./<path> -name .git -prune -o -type f -links 1 -exec grep -snHE -e <pattern> -- {} +`.
  - `find -type f` means grep never opens a symlink. BusyBox `grep -r`, which the plan first named, reads through symlinked *files*.
  - `-links 1` skips hardlinks.
  - BusyBox has no `--exclude-dir`, so results are always filtered in Go, failing closed: a line that doesn't parse, or that could be read as a path under `.git` or the denylist, is dropped.
  - A glob is matched in Go with `path.Match`. Results stop at 200 matches, with a count of the rest.
- **`apply_patch`** (`patch_parse.go`, `tool_patch.go`):
  1. The patch (at most 64 KiB, because it travels base64-encoded in argv) is parsed in Go, the way `git apply -p1` reads it.
  2. Every path is checked, including rename and copy targets.
  3. Quoted paths, binary patches, and any mode other than `100644` or `100755` (so no symlinks or submodules) are rejected.
  4. In the sandbox, `git apply --check --summary --numstat -z` is compared with the Go parse. Any path or mode git sees that the parser didn't validate is a rejection, and nothing is applied.
  5. After `git apply`, `git status` entries that changed must all be validated paths with no new symlink; otherwise the change is reverted and rejected.
- **Configured commands** (`run_tests`, `run_build`, `run_lint`): run exactly the argv from `toolchains.yaml`. Any arguments from the model are refused, not ignored, so the attempt shows in the audit.
- **`run_command`**: takes an argv from the model. Its timeout is clamped: zero, negative, or over the maximum becomes the maximum, then it is capped by the wall clock left. Without the clamp, the Docker sandbox would turn zero into its own full lifetime. A non-zero exit is a normal result.
- **`toolchains.yaml`**: keyed by repository full name, like `repos.yaml`, and decoded strictly. It ships a placeholder that fails validation. Every command tool the implementer role lists must be configured; a repository with nothing to lint uses `[true]`.

**The sandbox change** (`internal/sandbox/docker/exec.go`, `sandbox.go`, `doc.go`).
- Every sandbox command (`Exec`, `ReadFile`, `WriteFile`, `Diff`) ends with `killAll`, from a `defer` inside `run()`, on every exit path.
- `killAll` always signals PID 1, whose trap kills every other process.
- It confirms success only when the process list is the init plus one *new* `sleep 3600` child. A stray that kills the init's sleep can't pass for a quiet container.
- The file scripts refuse a symlink at any component, and a regular file with more than one link (`ln .git/config notes.txt` would otherwise let `write_file notes.txt` write `.git/config`).
- With nothing left running between serialized commands, nothing can swap a path component for a symlink during a file operation. That *narrows* the check-then-write race; it does not close it. See Left open.

**Persistence.**
- **`tool_calls`** (migration `00006`):
  - one row per call, `UNIQUE(attempt_id, session_key, step)`;
  - the raw argument bytes always, plus `jsonb` when Postgres accepts them;
  - the application role may only `SELECT` and `INSERT`.
  - `store.ToolCalls.Record` inserts with `ON CONFLICT DO NOTHING`, then reads the row back and compares every column, so a retry is absorbed and a disagreement is an error. `RejectionCount` counts rejected calls for an attempt.
- **`blob.Dir`** stores blobs under an `os.Root` handle.
  - Keys are `<uuid>/<sha256>`.
  - `Put` writes a temp file with `O_EXCL`, fsyncs it, renames it into place, and refuses a symlinked namespace.
  - `Get` refuses symlinks, re-hashes the bytes, and returns `ErrCorrupt` on a mismatch.

## Where to look

| File | What's in it |
|---|---|
| `internal/agent/runner.go` | `Run`, spec checks, what is offered |
| `internal/agent/loop.go` | The turn loop, step and token checks, repair, recording |
| `internal/agent/limits.go` | `repeatKey`, `canonicalArgs`, `truncate` |
| `internal/agent/prompt.go`, `role.go` | System prompt, task message, the `Implementer` role and its limits |
| `internal/agent/refusal.go`, `session_error.go` | `Refusal`, `classify`, `SessionError` |
| `internal/agent/record.go` | `ToolCallRecord`, `Recorder` |
| `internal/agent/path.go`, `internal/sandbox/path.go` | `checkPath`, the denylist, `CheckPath`, `FoldName` |
| `internal/agent/tool_*.go`, `patch_parse.go` | The ten tools |
| `internal/agent/report.go`, `toolchains.go` | Finish schema and validator, `LoadToolchains` |
| `internal/agent/agenttest/` | Fake sandbox over `os.Root`, scoped fake router |
| `internal/sandbox/errors.go` | The shared refusal sentinels |
| `internal/sandbox/docker/exec.go`, `sandbox.go`, `doc.go` | Kill after every command, hardlink refusal, the race write-up |
| `internal/sandbox/docker/dockertest/` | The Docker test harness, shared with `agent` |
| `internal/store/toolcalls.go`, `migrations/00006_tool_calls.sql` | The audit table and its writer |
| `internal/blob/` | `Store`, `Dir` |
| `deploy/toolchains.yaml` | Owner-edited commands, failing placeholder |

## What the tests prove

| Guarantee | Test |
|---|---|
| A session completes against the fake provider | `TestRunCompletesTask` |
| Each limit on its own | `TestLimitMaxStepsBoundary`, `TestLimitMaxTokensRunner`, `TestLimitAttemptBudgetIsTokenLimit`, `TestLimitMaxWallClock`, `TestLimitMaxRepeatErrors`, `TestLimitsMustBePositive`, `TestRunCommandTimeoutClamp` |
| Platform caps and a caller's cancel are not the agent's failure; distinct failures don't trip the repeat limit | `TestBudgetCapIsNotTokenLimit`, `TestParentCancelIsNotWallClock`, `TestDistinctFailuresDoNotTrip` |
| One repair, then `invalid_output` | `TestFinishRepairThenValid`, `TestFinishInvalidTwice`, `TestRepairIsOnlyOnceAcrossTurns`, `TestEndTurnWithoutFinish`, `TestFinishAlongsideOtherCalls`, `TestValidateReport` |
| Paths are refused, never cleaned, before the sandbox is reached | `TestCheckPathRejects`, `TestAttackSiblingPrefix` (agent and blob), `TestDenylistComponentAware` |
| No file tool follows a symlink or hardlink out of the workspace | `TestAttackToolsRefuseSymlinks`, `TestSearchDoesNotFollowSymlinkOut`, `TestAttackHardlinkRefused`, `TestAttackSwapBetweenValidationAndWrite` |
| No process outlives a sandbox command, so nothing can race a file operation | `TestAttackNoProcessSurvivesExec`, `TestAttackNoProcessSurvivesDiff`, `TestAttackStrayCannotSwapAfterExec` |
| `apply_patch` applies only what it validated | `TestApplyPatchRejects`, `TestApplyPatchGitSummaryMismatch`, `TestApplyPatchAppliesDocker` |
| `search` can't be turned into flags and hides `.git` and the denylist | `TestSearchArgumentInjection`, `TestSearchExcludesGitAndDenylist`, `TestListDirFiltersDenylist` |
| Only allowed tools are offered and reached | `TestOfferedToolsIntersectAllowlist`, `TestReadOnlyStripsMutatingTools`, `TestDisallowedCallNeverReachesSandbox`, `TestConfiguredCommandArgvExact` |
| Two calls with one failing; repeated identical calls (RL-6) | `TestTwoCallsOneFailing`, `TestRepeatedIdenticalCalls` |
| Every call reaches the audit table with its tool, arguments, duration and codes, before the next request | `TestToolCallsReachAuditTable` (Postgres), `TestEveryToolCallRecordedBeforeNextRequest` |
| A failed audit or blob write ends the attempt | `TestRecordFailureFailsClosed`, `TestBlobPutFailureFailsClosed` |
| Audit writes are idempotent and append-only | `TestRecordToolCallIsIdempotent`, `TestToolCallConflictMismatchErrors`, `TestAppRoleCannotAlterAppendOnlyRows` |
| A credential in an error reaches no audit row, no blob, and not Run's error text | `TestCanaryNotInAudit` |
| Blobs are verified on read | `TestGetVerifiesHash` |
| The issue text arrives unchanged; tool output arrives only as tool results | `TestIssueTextPassedUnchanged`, `TestToolOutputOnlyInToolResult` |
| The shipped `toolchains.yaml` fails; a missing implementer command fails at load | `TestLoadToolchains` |

## Left open

- **The check-then-write race is narrowed, not closed.** `FR-SEC29` asks for an `os.Root` handle, and none reaches into a container. Its fallback (resolve the parent, compare relative paths) is a different mechanism from the one used here: lexical checks, plus symlink and hardlink refusal in the scripts, plus no process surviving between serialized commands.
  - The residuals are a process in another PID namespace, which the sandbox as configured cannot create, and damage that stays inside the container.
  - The decision record for this deviation is owed.
- **Repeated rejections don't flag the attempt yet.** `RejectionCount` exists; ticket 7's gate uses it.
- **`store` imports `agent`** for `ToolCallRecord`, so the persistence package links the agent loop. A hand-reviewed `contracts.ToolCallRecord` or a small `audit` package would undo that before ticket 7 reads the rows.
- **Nothing is wired yet.** Ticket 8 has to:
  - build the scoped router and the blob directory;
  - load `toolchains.yaml`;
  - choose the `SessionKey`;
  - supply a denylist; none has a configuration source yet.
- **Smaller limits:**
  - `search` uses `find` and grep, not ripgrep.
  - `apply_patch` is capped at 64 KiB because `ExecRequest` has no stdin.
  - `apply_patch` judges `git status` entries, not file contents, and its revert has documented limits (`tool_patch.go`).
  - Every sandbox command costs about 30 ms more for the kill.
  - The audit insert needs Postgres 16 (`pg_input_is_valid`).
  - A prompt-injection fixture repository is left to Milestone 2.
- **Decision records owed:**
  - the FR-SEC29 deviation;
  - the blob store layout and keys;
  - `toolchains.yaml` and its keying;
  - find and grep instead of ripgrep;
  - `apply_patch` via argv and the git cross-check;
  - where the audit record type lives;
  - `tool_result` as the provenance label for tool output.

## Check your understanding

1. The model sends `write_file` for `notes.txt`, which a previous `run_command` made a hardlink to `.git/config`. Which layer refuses it, and what does the audit row say?
2. Why does `search` use `find -type f … -exec grep` rather than `grep -r`, and why is the Go-side filter still needed?
3. A reply holds three calls and `MaxSteps` has one left. What happens to each call, and what does the model see next?
4. The feature request's token cap refuses a model call. Why is that not `token_limit`, and what does ticket 8 do with it?
5. Why is the audit row written before the result goes back to the model, and why with a context detached from the session's?
6. What would let a component be swapped for a symlink between the script's check and its write, and why can't that happen as the sandbox is configured?
