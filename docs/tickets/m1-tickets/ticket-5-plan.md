# Ticket 5 — Agent loop and tools: multi-agent build plan

## TL;DR
- **What gets built:**
  - `internal/agent`: the runner, the 10 tools, limits, the finish report with one repair, and the audit.
  - A new `internal/blob` package, a `tool_calls` table (migration 00006; 8a takes 00005), and `deploy/toolchains.yaml`.
  - One change to the Docker sandbox: every sandbox command is followed by killing any process other than init.
  - No contract changes and no new dependencies.
- **Gaps filled:**
  - No `os.Root` reach into the container. The swap race is *narrowed*, not closed: no process survives a sandbox command, the file scripts refuse symlinks and hardlinks, and paths are checked lexically. Any residual damage stays inside the container. A decision record names this as a **deviation** from FR-SEC29's fallback, and lists the residuals.
  - Commands come from runner config, loaded from `toolchains.yaml`.
  - Blob store: `internal/blob`, built on `os.Root`.
  - The audit row type lives in `agent` and `store` implements it.
- **Agents:**
  1. The lead writes the shared seams, with a stub for every owned file.
  2. Four Fable agents work in parallel, each in its own worktree: W1 paths and file tools, W2 store and blob, W3 sandbox race, W4 commands and finish.
  3. The lead merges, then one agent builds the runner loop and end-to-end tests.
  4. `make check`, then a verifier agent (traceability plus mutation checks), then `/review-ticket 5`.
- **Done-when:** every item maps to named tests (see the table). The race tests run against the real Docker sandbox. The audit test runs against real Postgres as `conclave_app`.
- **Deferred, stated in the PR:**
  - Flagging repeated rejections is left to gating (ticket 7).
  - `search` uses grep instead of ripgrep.
  - `apply_patch` is capped at about 64 KiB, because `contracts.ExecRequest` has no stdin and the patch travels in argv.
  - A prompt-injection fixture repository is left to Milestone 2 ("Not in this milestone" in `milestone-1.md`).

## Context
Ticket 5 (`docs/tickets/milestone-1.md:80-90`) implements `contracts.AgentRunner` and the Milestone 1 implementer tools in `internal/agent` (only `doc.go` exists today). Requirements: FR-SEC3, FR-SEC24, FR-SEC29. Review list: RL-6, RL-11.

Exploration found four things the ticket assumes but the repo does not have:
1. **No `os.Root` reach.** The Docker workspace is a tmpfs inside the container. ReadFile and WriteFile use shell scripts that refuse symlinks in every path component but race with a concurrent swap (`internal/sandbox/docker/doc.go:46-57`, "File operations").
2. **No audit table.**
3. **No blob store.**
4. **No per-repo command config** (FR-I4).

User decisions made during planning:
- **Narrow the race in the sandbox:** kill stray processes after every sandbox command, refuse hardlinks, and validate paths lexically in the agent layer. FR-SEC29's fallback is "resolve the parent through symlinks, then compare relative paths, separator-aware". This plan uses a different mechanism, so the decision record calls it a deviation, not the fallback.
- **Commands:** `agent.Commands` on the runner config, loaded from a new `deploy/toolchains.yaml`.
- **Blob store:** a new `internal/blob` package.
- **Build process:** Fable agents, one per git worktree, working on separate files.

No contract changes and no new dependencies.

### Scope beyond `internal/agent` (approved or flagged)
- `internal/sandbox/docker/exec.go`, `sandbox.go`, `doc.go`, and its tests: kill after every command, hardlink refusal (approved). Kill-after-Exec was approved; extending it to every `run()` and adding hardlink refusal follows from the same decision. **Flagged.**
- `internal/sandbox/path.go`, `errors.go`, `doc.go`: neutral sentinels (`ErrInvalidPath`, `ErrNotFile`, `ErrFileTooLarge`, `ErrOutputTooLarge`) plus the shared `CheckPath`. Docker aliases them so the runner can tell a refusal the model should see from a sandbox failure. `doc.go` (`internal/sandbox/doc.go:1-13`) currently describes only tree preparation and must be updated. **Flagged:** a small widening that follows from the sandbox decision.
- `internal/sandbox/docker/dockertest`: exports the Docker test harness that today is unexported in `docker_test` (`main_test.go:22-60`), so `agent` tests can use it. **Flagged.**
- `internal/store`: migration 00006 and `toolcalls.go`.
- New `internal/blob` package.
- `deploy/toolchains.yaml`.
- One line in the ARCHITECTURE.md layout for `internal/blob`.
- **Audit row type:** `agent.ToolCallRecord` plus the `agent.Recorder` interface, which `store.ToolCalls` implements. store imports agent and agent never imports store. This avoids changing contracts. If you'd rather have `contracts.ToolCallRecord` to match `CostEvent`, that becomes a hand-reviewed contract proposal before L0.

### Pre-flight housekeeping
- **Ticket 8a lands first.** It makes `AttemptID` and `FeatureRequestID` mandatory on budgeted scopes, wraps cap refusals in a `BudgetError` that says which budget was hit, and takes migration 00005. After 8a merges, rebase `ticket/5` onto `main`. If ticket 5 has to go first instead, swap the migration numbers and drop the `BudgetError` mapping below until 8a.
- `ticket/5` already sits on `chore/ci`'s tip (04e145e), so the `test-ci` "fail, don't skip" behaviour applies. Merge `chore/ci` to `main` before opening PR 5, or PR 5 carries four unrelated CI commits.
- The accidental `---` → `--` in `docs/tickets/milestone-1.md` has been reverted as part of the ticket 8 split update.

## Design decisions (fixed for all agents)
- **Runner per attempt:** `agent.New(Config{Router contracts.Router, Recorder, Blobs blob.Store, AttemptID, SessionKey, Commands, Denylist, OutputCap, MaxCommandTimeout})`.
  - No `RepoID` on Config. The runner uses `in.RepoID`, so the two can't disagree.
  - The caller builds the Router. In ticket 8 the activity calls `llm.Router.Scope` with the same `AttemptID` and the feature request ID, and passes the resulting `*llm.Scoped` (which satisfies `contracts.Router`; `*llm.Router` does not). State this in `agent/doc.go`.
  - Each request's `PromptVersion` is taken from `in.Spec` (decision 0006).
  - `SessionKey` must be unique per Run execution, e.g. workflow/activity/attempt-number.
  - Limits come from `in.Spec.Limits`; note that `in.Limits` is `Constraints`.
  - All four limits must be > 0, otherwise `New`/`Run` returns a config error.
- **Contract details the runner must honour (no contract change):**
  - Fill `SessionOutput.Steps` (`session.go:41`) with every tool call, in order.
  - Every tool implements `contracts.Tool` (`tool.go:72-82`).
  - Include `in.Profile` and, on a retry, `in.PriorFailure` in the prompt.
  - Support `read_file`'s line range and `search`'s glob argument from `docs/agent-roles.md`. The glob is matched in Go against the results with `path.Match`, never passed to grep.
- **Tools offered** = `Spec.Tools` ∩ registry.
  - `ReadOnly` strips write_file, apply_patch and every `run_*` tool.
  - A Spec tool missing from the registry, or a command tool with no configured command, makes `New` fail.
  - A call to a tool not on the list gets a refusal result. It is audited and counted as a step, and never reaches the sandbox.
- **Implementer RoleSpec value** lives in `agent/role.go`. It has the 10 M1 tools; `request_dependency` waits for M3.
- **Loop:** `Run → turn → execCalls → execOne → record`, split to stay under the funlen and gocognit limits.
  - Append the assistant message with the original ToolCalls unfiltered (faketest checks the IDs).
  - Run calls in order, and answer every ID.
  - Tools return a `*Refusal` for anything the model should see. A sandbox sentinel also goes to the model. Any other error is `sandbox_error`, returned with a wrapped error.
- **Limits:**
  - Steps are checked before each call. Unexecuted calls are recorded as `not executed: step_limit`.
  - Tokens are tracked from `Response.Usage` against `MaxTokens`, and `req.MaxTokens` is set to the remaining amount.
  - **Budget errors are split by kind.** Use 8a's `BudgetError` discriminator:
    - The attempt budget, or the runner's own `MaxTokens`, ends the session with `Reason = token_limit`.
    - The feature-request cap and the monthly cap are **not** agent failures. `Run` returns them as a wrapped error (still `errors.Is(err, llm.ErrBudgetExceeded)`), so the ticket 8 activity can route them to `needs_attention` with `reason: budget_cap`.
  - Wall clock uses `context.WithTimeoutCause`. A parent cancel is an error, not `wall_clock`.
  - Repeat errors are counted as totals per key (tool + canonicalised args + exit code + output hash).
- **Finish:**
  - One definition produces both the JSON schema and the validator. The validator decodes to a map so it can require fields, rejects unknown fields, trailing data and null arrays.
  - The first invalid report gets an error result carrying the validation message; this is the one repair. The second gives `invalid_output`.
  - An `end_turn` or `max_tokens` reply with no tool call uses up the repair through a "call finish" nudge.
  - A `finish` sent alongside other calls is rejected, but that does not use up the repair.
- **Paths:**
  - `CheckPath` rejects:
    - abs paths, `..`, dots-only components (`....//`), `%XX`, backslash
    - NUL, C0 and C1 control characters, bidi overrides, invalid UTF-8
    - `.git` compared case-insensitively, including variants with trailing dots or spaces, with Unicode characters HFS+ ignores (U+200C and similar), and the NTFS short name `git~1`. These matter when the owner checks the branch out on macOS or Windows.
  - The denylist matches whole path components.
  - Rejections are audited with `rejected=true`, and `RejectionCount(attemptID)` is exposed. Sandbox symlink and hardlink refusals (script exit 3) count as rejections too. Flagging the attempt is deferred to gating (ticket 7); say so in the PR.
- **Exec-based tools:**
  - `list_dir` and `search` walk each component of the path with `[ -L ]`, like `readScript`.
  - `search`:
    - Uses `grep -rnE -e <pattern> -- <path>`, not ripgrep, because the image doesn't guarantee rg. Use `-r`, never `-R`.
    - Always puts `--` before the path, so a path starting with `-` can't become a flag.
    - Excludes `.git` and denylisted paths. Probe busybox for `--exclude-dir` and `-I`. Whether or not the probe succeeds, filter the results in Go and fail closed: drop any hit under `.git` or the denylist, and never skip the filter silently.
    - Results are truncated with a count.
  - `list_dir` filters `.git` and denylisted entries the same way.
  - `apply_patch`:
    - Parses headers in Go and validates every path, including rename/copy and traditional (`---`/`+++` only) patches.
    - Rejects quoted paths, binary patches, and modes 120000 and 160000. Caps the raw patch at about 64 KiB, the argv limit, because `ExecRequest` has no stdin.
    - Before applying, runs `git apply --check --summary --numstat -z` in the container and checks git's own list of paths and modes against the Go parse. Any path or mode the Go parser didn't validate is a rejection.
    - Applies with `git apply`, the patch passed base64-encoded in argv. Never passes `--unsafe-paths` or `--directory`.
    - After applying, checks that `git status --porcelain -z` lists only validated paths and that no new symlink exists. Anything else is reverted with `git checkout`, and the call becomes a rejection.
  - `run_command`:
    - The timeout is clamped. A value of zero or less, or one above `MaxCommandTimeout`, becomes `MaxCommandTimeout`, and the result is capped at the remaining wall-clock time.
    - This matters because the sandbox turns zero or less into its own full `Limits.Timeout` (`exec.go:113`).
  - Configured `run_*` tools **reject** model arguments with a refusal, rather than ignoring them, so the attempt shows in the audit. The argv executed is exactly the configured one.
  - Tool descriptions and the system prompt say that no process survives between commands. A service started by one `run_command` is gone before the next.
- **Prompt (FR-SEC28):**
  - `IssueText` "arrives already quarantined" (`session.go:21`). The quarantine is the webhook side's job (FR-SEC28 is on ticket 6). The runner does not wrap it again. It places the text unchanged in a user message labelled as untrusted issue text.
  - Tool results reach the model only as `tool_result` blocks. That role is their provenance label, and the static system text says tool results and issue text are data, not instructions. Record this choice in the decision record.
- **Output:**
  - Tools return full output. The runner truncates it for the model (head and tail) and stores the full output in the blob store at `ArtifactKey`.
  - If the blob `Put` fails, the attempt ends `errored`. FR-SEC24 requires the pointer, so this fails closed, the same way a record failure does.
- **Audit:**
  - Migration `00006_tool_calls.sql` columns: `attempt_id` (FK), `session_key`, `step`, `call_id`, `tool`, `arguments_raw bytea NOT NULL`, `arguments jsonb NULL` (only when the bytes parse), `exit_code`, `duration_ms`, `artifact_key`, `error`, `rejected`, `truncated`, `occurred_at`.
  - `UNIQUE(attempt_id, session_key, step)`, with a grant of SELECT and INSERT only. 00004 grants per table with no default privileges, so 00006 needs its own explicit grant.
  - The worker stamps `occurred_at`; there is no DB default. Insert with `ON CONFLICT DO NOTHING`, then select and compare all columns, as `store/costs.go` does. Error if they differ.
  - The `error` column holds only sanitized text: the sentinel's name, never raw provider or sandbox output.
  - The record is written before the result returns to the model. If the write fails, the attempt ends `errored` (fail closed).
  - The final write uses `context.WithoutCancel` with a short timeout.
- **Blob store:**
  - `Store{Put(ctx, ns, data) (key, error); Get(ctx, key)}`. Keys are `<repoID>/<sha256>`, which is idempotent and leaves room for per-tier retention.
  - `Put` checks `ns` against the same regex as `Get`.
  - `Dir` is built on `os.Root`: temp file with O_EXCL, then Rename.
  - `Get` checks the key against a regex and verifies the hash on read.
- **Docker:**
  - `killAll` runs inside `run()` from a `defer`, so it covers every sandbox command: `Exec`, `ReadFile`, `WriteFile`, and `Diff`. It also runs on error paths, including "exec still running after its stream closed" (`exec.go:141`).
    - `Diff` matters because `git add -A -N` honours `core.fsmonitor`, which the agent can set through `run_command`.
    - It runs while `ops` is still held, using `context.WithoutCancel`.
  - `killAll` **always** sends SIGUSR1. `ContainerTop` only confirms the result afterwards; a process count is never a reason to skip the signal. A stray process can kill init's `sleep` child, so the count can't be trusted beforehand.
    - The first confirmation poll is at 10 ms, not 100 ms, to keep file operations fast.
  - The read and write scripts refuse a regular file whose link count is above 1 (exit 3). Otherwise `ln .git/config notes.txt` followed by `write_file notes.txt` would write `.git/config`.
  - `TestReadFileTooLargeIsAPlainError` must be rewritten, since it relies on a surviving `sleep` (`sandbox_test.go:299-305`).
  - Update `docker/doc.go:46-57` and the comment on the swap-race test (`attack_test.go:322-327`) to describe the new mechanism and its residuals.
  - Side benefit: daemons started during install no longer survive into the agent phase. Install steps go through `sb.Exec` (`provider.go:137`), so each step must be self-contained too. Check the Election Tally install and commands against this.
- **toolchains.yaml:** keyed by repo `full_name`, the same as `deploy/repos.yaml`. It ships a placeholder entry that fails validation, following the `repos.yaml` convention, rather than a real Election Tally entry.

## Multi-agent execution

All build agents use **model: fable** (the ticket's model column says F). Each agent gets in its prompt:
- CLAUDE.md hard rules
- its exact file ownership
- the L0 seam signatures
- the tests it must deliver, by name
- "do not touch contracts or go.mod; stop and report if you need to"
- the two-strikes rule
- a requirement to finish with `go build ./...`, `go test -race` on its packages, and `golangci-lint run` on its packages

### Phase L0: lead (me), sequential on `ticket/5`
Write compiling stubs for every seam, then commit. This is the base each worktree branches from. **Every file a worker owns exists as a stub after L0.** A worker only replaces bodies in its own files and never edits a shared file.
- `internal/sandbox/path.go`: `CheckPath` and `ErrInvalidPath`, moved from `docker/path.go` with its tests. Docker keeps an alias.
- `internal/sandbox/errors.go`: the sentinels. `internal/sandbox/doc.go`: updated responsibility.
- `internal/sandbox/docker/dockertest`: the exported harness (`New(t)`, the test image, reaping), moved out of `main_test.go`.
- `internal/blob/{doc.go,blob.go}`: the interface, `ErrNotFound`, `ErrInvalidKey`.
- `internal/agent`:
  - `record.go` (ToolCallRecord, Recorder), `config.go` (Config, Commands, New), `refusal.go`.
  - `registry.go`, which only *references* constructors; it defines none.
  - One stub file per tool: `tool_read.go`, `tool_list.go`, `tool_search.go`, `tool_write.go`, `tool_patch.go`, `patch_parse.go`, `path.go` (W1); `tool_command.go`, `toolchains.go` (`LoadToolchains(io.Reader)`), `report.go` (`validateReport`, `finishSchema`), `tool_finish.go` (W4).
  - Stubs for W5's files too: `runner.go`, `loop.go`, `limits.go`, `prompt.go`, `role.go`.
- `internal/agent/agenttest/{doc.go,...}`:
  - A fake `Sandbox` over `os.Root` with an `ExecFunc` hook and a call log.
  - `Router(t, turns...) (*llm.Scoped, *faketest.Provider)`. It wraps `llm.New` and `faketest`, and passes UUID `AttemptID` and `FeatureRequestID` plus valid 8a cap values. The provider is returned so tests can call `Remaining()`.

### Phase 1: four agents in parallel, each in its own worktree
| Agent | Owns | Delivers |
|---|---|---|
| **W1 paths and file tools** | `agent/{path,tool_read,tool_list,tool_search,tool_write,tool_patch,patch_parse}.go` and tests, `agent/tools_docker_test.go` (uses `dockertest`) | `TestCheckPathRejects` (including case, Unicode and `git~1` variants), `TestDenylistComponentAware`, `TestAttackSiblingPrefix` (agenttest; **asserts zero sandbox calls and that the `ErrInvalidPath` came from CheckPath**), `TestAttackToolsRefuseSymlinks` (docker; all 5 tools), `TestApplyPatchRejects` (120000 in a traditional diff, a binary patch, a rename into `.git`, a target under a symlinked directory the repo ships, a path git sees that the parser didn't), `TestApplyPatchGitSummaryMismatch`, `TestSearchArgumentInjection` (path `-f`), `TestSearchExcludesGitAndDenylist`, `TestSearchDoesNotFollowSymlinkOut` (docker), `TestListDirFiltersDenylist` |
| **W2 persistence** | `store/migrations/00006_tool_calls.sql`, `store/toolcalls.go` and test, edits to `migrate_test.go` (`wantTables`) and `appendonly_test.go`, `blob/dir.go` and test | Idempotent insert, `TestToolCallConflictMismatchErrors`, append-only grant test, blob `TestAttackSiblingPrefix` (`<root>` vs `<root>-evil`), hash verification, key and `ns` regex |
| **W3 sandbox race** | `sandbox/docker/{exec.go,sandbox.go,doc.go,attack_test.go,sandbox_test.go}` | Kill in `run()`; always signal; hardlink refusal. Tests: `TestAttackNoProcessSurvivesExec` (cases: `setsid` with a double fork, `nohup` with all fds closed, a stray that kills init's `sleep`), `TestAttackNoProcessSurvivesDiff` (a `core.fsmonitor` hook), `TestAttackStrayCannotSwapAfterExec` (a background swap loop is dead when Exec returns, then repeated WriteFiles never land in `/tmp`), `TestAttackHardlinkRefused` (read and write), `TestAttackSwapBetweenValidationAndWrite` (an existing symlink is refused), the rewritten too-large test |
| **W4 commands and finish** | `agent/{tool_command,toolchains,report,tool_finish}.go` and tests, `deploy/toolchains.yaml` (placeholder entry) | Configured commands refuse model arguments and run exactly the configured argv (`TestConfiguredCommandArgvExact`); `TestRunCommandTimeoutClamp` (0, -1, 10h, and above the remaining wall clock); strict toolchain loader, placeholder fails validation; `TestValidateReport` table |

The lead merges W2, W3, W1 and W4 in that order into `ticket/5`. Because every file was stubbed in L0 and ownership is disjoint, there should be no conflicts. A conflict means ownership leaked, so stop and check which worker touched a file it didn't own. Run `make build` and `make test` after each merge.

### Phase 2: one agent, W5 runner (sequential, after the merge)
Owns `agent/{runner,loop,limits,prompt,role}.go` and the tests `runner_test.go`, `limits_test.go`, `finish_test.go`, `prompt_test.go`, and `store/toolcalls_agent_test.go`. That last one is in the external `store_test` package, so it reuses `TestMain`, `seed` and `testTx` (which already runs as `conclave_app`).
- **Prompt:** the system text is static. The task, profile, prior failure and issue text go in a user message. The issue text goes in unchanged (see Prompt above).
- **Fixtures:** in `agent/testdata/*.json`. Every turn sets `input_tokens` and `output_tokens`, because zero-usage turns are billed at the full reservation.

### Phase 3: verification and review
1. The lead runs `make check` (vet, lint, tidy, test, workflowcheck, govulncheck) and `make test-ci`, with Docker and Postgres up.
2. **Verifier agent** (fresh, read-only plus test runs):
   - Builds the Done-when traceability matrix below from real test names.
   - Runs mutation checks, each reverted afterwards. Each one must make a named test fail:
     - remove kill-after-command → `NoProcessSurvivesExec`, `NoProcessSurvivesDiff`
     - restore the count-based skip in killAll → `NoProcessSurvivesExec` (stray kills init's sleep)
     - remove the hardlink check → `HardlinkRefused`
     - remove `[ -L ]` from the list_dir and search scripts → `ToolsRefuseSymlinks`
     - remove the runner's token check → `MaxTokensRunner`
     - map every `ErrBudgetExceeded` to `token_limit` → `BudgetCapIsNotTokenLimit`
     - remove the repair counter → `FinishInvalidTwice`
     - replace `CheckPath` with a prefix check → `CheckPathRejects`, `SiblingPrefix` (agenttest)
     - remove the 120000 rejection, or the git summary check → `ApplyPatchRejects`, `ApplyPatchGitSummaryMismatch`
     - drop `--` from grep → `SearchArgumentInjection`
     - drop the Go-side denylist filter in search → `SearchExcludesGitAndDenylist`
     - drop the timeout clamp → `RunCommandTimeoutClamp`
     - skip `Record` → `EveryToolCallRecorded`
     - turn the record-failure path into log-and-continue → `RecordFailureFailsClosed`
     - turn a blob `Put` failure into log-and-continue → `BlobPutFailureFailsClosed`
     - make the recorder's conflict comparison a no-op → `ToolCallConflictMismatchErrors`
     - remove hash verification in blob `Get` → the blob hash test
3. `/review-ticket 5` (ticket-reviewer, Opus). Fix the must-fix findings, rerun `make check`, then commit with Conventional Commits and the PR description. The PR covers RL-6, RL-11, the FR-SEC29 deviation and its residuals, and the deferred rejection flag.
4. **Decision records owed** (you write them):
   - kill-after-command plus hardlink refusal as a deviation from FR-SEC29's fallback. List the residuals: a process in another PID namespace (not possible as configured), and damage limited to the container.
   - the blob store layout and keys
   - toolchains.yaml and its keying
   - grep instead of rg
   - apply_patch via `git apply` in argv, and the git cross-check
   - the audit record type's home
   - tool_result as the provenance label for untrusted tool output

## Done-when traceability (must all exist and pass)
| Done-when | Tests |
|---|---|
| 1. Loop completes against the fake provider | `TestRunCompletesTask`: report parsed, Reason empty, `Remaining()==0`, file written, Usage, Provenance and Steps set |
| 2. Each limit tested separately | `TestLimitMaxStepsBoundary` (N passes, N+1 fails), `TestLimitMaxTokensRunner` (large Scope budget, small MaxTokens, `Remaining()>0`), `TestLimitAttemptBudgetIsTokenLimit`, `TestBudgetCapIsNotTokenLimit` (feature-request and monthly caps return a wrapped error, no Reason), `TestLimitMaxWallClock`, `TestParentCancelIsNotWallClock`, `TestLimitMaxRepeatErrors`, `TestDistinctFailuresDoNotTrip`, `TestLimitsMustBePositive`, `TestRunCommandTimeoutClamp` |
| 3. Path escapes: symlink, swap race, sibling prefix, `....//` | `TestCheckPathRejects`, `TestAttackToolsRefuseSymlinks`, `TestAttackSwapBetweenValidationAndWrite`, `TestAttackStrayCannotSwapAfterExec`, `TestAttackNoProcessSurvivesExec`, `TestAttackNoProcessSurvivesDiff`, `TestAttackHardlinkRefused`, `TestAttackSiblingPrefix` (agenttest and blob), `TestApplyPatchRejects`, `TestApplyPatchGitSummaryMismatch`, `TestDenylistComponentAware`, `TestSearchArgumentInjection`, `TestSearchExcludesGitAndDenylist` |
| 4. Invalid finish → one repair → `invalid_output` | `TestFinishRepairThenValid`, `TestFinishInvalidTwice`, `TestRepairIsOnlyOnceAcrossTurns`, `TestEndTurnWithoutFinish` |
| 5. Every tool call in the audit table | `TestToolCallsReachAuditTable` (Postgres; covers allowed, refused, rejected path, sandbox symlink refusal, failing command, invalid then valid finish; contiguous steps; blob key resolves), `TestEveryToolCallRecordedBeforeNextRequest`, `TestRecordFailureFailsClosed`, `TestBlobPutFailureFailsClosed` |
| FR-SEC3 / RL-11 | `TestOfferedToolsIntersectAllowlist`, `TestReadOnlyStripsMutatingTools`, `TestDisallowedCallNeverReachesSandbox`, `TestConfiguredCommandArgvExact` |
| RL-6 | `TestTwoCallsOneFailing`, `TestRepeatedIdenticalCalls` |
| FR-SEC24 | Audit columns complete; blob pointer set whenever output was truncated; `TestCanaryNotInAudit` (a router or sandbox error carrying a `canary_*` value appears in neither `tool_calls` nor any blob) |
| FR-SEC28 (runner side) | `TestIssueTextPassedUnchanged`, `TestToolOutputOnlyInToolResult` |

## Verification
- `make check` and `make test-ci` both green, with no skipped integration tests.
- `go test -race ./internal/agent/... ./internal/blob/... ./internal/store/... ./internal/sandbox/...` green.
- Mutation checks above confirmed by the verifier.
- No test reaches a real provider (faketest only). No real keys in fixtures (canary format only). `internal/agent` doesn't import `internal/store`, which would be an import cycle.
