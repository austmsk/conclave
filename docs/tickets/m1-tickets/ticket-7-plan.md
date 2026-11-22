# Ticket 7 — Gates and artifact integrity: build plan

## TL;DR
- **What gets built:**
  - `internal/gate`: freezing, the artifact envelope, staging, and the 8 Milestone 1 gates plus a `tool_calls` flag, all recorded idempotently.
  - `store` persistence for gate results.
  - Migration 00007.
  - `deploy/gates.yaml`.
  - A platform `deploy/gitleaks.toml`.
- **The artifact** is an envelope binding the diff to its base: `conclave-artifact v1\nbase <sha>\n\n<diff bytes>`. Its SHA-256 hex is the blob key, so `blob.Get` re-verifies it on every read. A diff rebased onto a new base gets a new hash and must be re-gated (ticket 9).
- **Freeze is `Diff` read twice.** The provider already kills every process after every command. The contract gains that guarantee as a doc comment. There is no `Kill` method.
- **No gate reads the agent's workspace after the freeze.** Both modes stage the same way:
  1. Wipe the workspace, `.git` included.
  2. Stream the clean prepared tree back in.
  3. Apply the recorded diff with git config isolated.
  4. Re-diff, and require byte equality with the recorded diff.
  - `reset_in_place` stages into the agent's container. `clean_room` stages into a fresh one.
- **gitleaks runs on the worker**, pinned by SHA-256. It gets an empty environment, an empty temp directory and the platform config. Binary content fails the gate.
- **Tests must report.** The tests gate requires a JUnit XML report showing at least one test ran and none failed. An exit code alone is not trusted.
- **`errored` vs `failed`** is decided by classifying errors, not by a baseline run (deferred to Milestone 2 with FR-A4). An `errored` result writes no rows.
- **Contract changes for hand review:**
  - a doc guarantee on `Sandbox`;
  - `GateToolCalls`;
  - moving `ToolCallRecord` into contracts, with an alias left in `agent`.

  No new Go dependencies.

## Context
Ticket 7 is specified in `docs/tickets/milestone-1.md` ("Ticket 7 — Gates and artifact integrity").
- **Requirements:** FR-A1, FR-A3, FR-SEC25, FR-SEC27.
- **Review list:** RL-2.
- **Also touches:** FR-SEC8 (protected paths), FR-SEC24 and FR-SEC29 (tool calls, repeated rejections, deferred here by ticket 5), and FR-I4 (no language-specific logic hardcoded in the platform).

`internal/gate/` holds only `doc.go`.

**How this plan was made.** Three independent plans were written from a shared baseline: plan C from ticket 6, meaning security-first, fail closed, no new Go dependencies, and gitleaks only as a binary. Each optimized for something different:
- **Plan 1** aimed for the simplest correct design.
- **Plan 2** aimed for the strongest artifact integrity.
- **Plan 3** aimed for the least rework in Milestones 2 and 3.

This document is the synthesis the owner chose on 2026-10-24:
- **The core is plan 3:** re-streamed staging, the envelope hash, and additive seams.
- **From plan 2:** the double `Diff` read, the write-once artifact hash, `Verify` for ticket 9, and the threat table.
- **From plan 1:** the errored-vs-failed convention, and keeping the API down to two calls.

**Two gaps in merged code** were found while planning. This ticket closes both:
1. **`conclave_app` can `UPDATE gate_results`** (`internal/store/migrations/00004_app_role.sql:33-35`). The push path trusts `passed`.
2. **`Diff` can be made to misdescribe the change.** It runs `git diff` without `--no-textconv`, under a `.git/config` the agent can edit with `run_command` (`internal/sandbox/docker/sandbox.go:217`). Re-streamed staging makes this harmless: the recorded bytes are applied to a clean base, and that tree is what gets gated and later pushed. This plan does not change `Diff`.

### Owner decisions (2026-10-24)
- **Contract batch approved for hand review.** The ticket session still shows the exact diff before committing it:
  - the `Sandbox` doc guarantee;
  - `GateToolCalls`;
  - moving `ToolCallRecord`, with an alias.
- **No baseline run in Milestone 1.** It arrives with flaky-test detection (FR-A4) in Milestone 2.
- **The tests gate requires a JUnit XML report.**
- **Binary content fails the secrets gate.**
- **Staging uses plan 3's re-stream approach.** No `ResetWorkspace` contract method.
- **A failing static gate skips the command gates.**

### Scope beyond `internal/gate`. Ask before doing anything marked *flagged*.
- `internal/contracts`: the approved batch above.
- `internal/agent/record.go`: replace the struct with `type ToolCallRecord = contracts.ToolCallRecord`.
- `internal/store`:
  - new `gates.go` and `gates_test.go`;
  - `toolcalls.go` switches to the contracts type;
  - the `var _ agent.Recorder` assertion moves to `toolcalls_agent_test.go`, so `store` stops importing `agent`.
- `internal/store/migrations/00007_gate_integrity.sql`.
- **Flagged:** move `writeTree` from `internal/sandbox/docker/archive.go:49` to `internal/sandbox`, so the gate and a later Daytona backend share it. This is a refactor of ticket 3 code with no change in behaviour. If refused, the gate keeps a copy.
- **Flagged:** in `.github/workflows/ci.yml`, install the pinned gitleaks and check its SHA-256, so the real-binary test runs in CI.
- `deploy/gates.yaml` (a placeholder that fails validation), `deploy/gitleaks.toml`, `deploy/local/gates.yaml` (git-ignored ETS values), and the ETS test command in `deploy/local/toolchains.yaml`.
- Docs:
  - `docs/guide/tickets/07-gates-and-artifact-integrity.md`;
  - the index row in `docs/guide/tickets/README.md`;
  - overview step 8.

  The owner writes the decision records (see 0001). Topics:
  - artifact identity: the envelope and re-streamed staging;
  - gitleaks on the worker;
  - error classification without a baseline.

## Design decisions (fixed)

**1. Freeze.**
- `gate.Freeze(ctx, sb, blobs, repoID, base)` calls `sb.Diff` twice and requires identical bytes, otherwise it returns `ErrUnstableDiff`.
- It refuses a diff above a hard byte cap (`ErrArtifactTooLarge`). That is the attempt's own failure, so the attempt is failed and no rows are written.
- It builds the envelope, calls `blobs.Put`, and returns `Artifact{RepoID, Base, Hash, Key, Size}`.
- The provider's kill-after-every-command guarantee (`internal/sandbox/docker/doc.go:71-87`) becomes a documented `Sandbox` guarantee.
- Nothing after `Freeze` reads the agent's workspace.

**2. `ToolCallRecord`.** Move it verbatim from `internal/agent/record.go:14-47` to `internal/contracts/record.go`, and leave a type alias in `agent`. The gate reads `RejectionCount` (`internal/store/toolcalls.go:111`) through a small interface it defines itself, and never imports `agent`.

**3. Staging.** `stage.go`, the same for both modes:
1. Find the sandbox:
   - `reset_in_place` uses the agent's sandbox.
   - `clean_room` uses `Input.NewSandbox(ctx)`, a factory from the caller. That hides that `CreateWithInstall` exists only on `*docker.Provider` (`provider.go:121`).
2. Empty `/workspace`, `.git` included, and `/tmp`, after `chmod -R u+rwX`. Check that both are empty.
3. Check the prepared tree on the worker:
   - `sandbox.VerifyTree`;
   - `sandbox.HeadCommit` must equal `Artifact.Base`.

   Then tar it with `writeTree`, `WriteFile` the tar into the workspace, move it to `/tmp`, and extract. `/home/agent` is kept, so caches stay warm; that is accepted threat T8a, and any flag forces `clean_room`.
4. `WriteFile` the diff, move it to `/tmp`, and run `git apply --binary` with `GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null HOME=/nonexistent`.
5. **Round trip.** Re-diff with the same command `Diff` uses (`sandbox.go:217`), plus `--no-textconv`, in the same isolated environment. Require byte equality with the recorded diff.

If the apply or the round trip fails, build, lint and tests are recorded as `Passed=false`, with Detail "not run: artifact does not reproduce on base". The prepared tree must still exist on the worker at gating time; ticket 8 keeps it until the attempt is gated.

**4. Hash.** SHA-256 hex of the exact envelope bytes, with no normalization. Every `GateResult` is built only by `newResult(art, gate, mode)`, which sets `ArtifactHash` from the artifact.

**5. gitleaks.**
- **Pinning.** The worker uses a binary pinned by `{path, version, sha256}` in `deploy/gates.yaml`, checked at startup and before each run.
- **Command.** `gitleaks stdin --config deploy/gitleaks.toml --no-banner --redact --ignore-gitleaks-allow --exit-code 99 --report-format json --report-path <tmp>/r.json`.
- **Isolation.** Run with `cmd.Env` empty, `cmd.Dir` an empty temp directory, a timeout and an output cap.
- **Config.** `deploy/gitleaks.toml` extends the defaults and adds a `canary_*` rule (FR-SEC20).
- **Input.** Only the added lines, under a header naming each path.
- **Outcomes:**
  - exit 0 with `[]` passes;
  - exit 99 with findings fails, keeping only rule ID and path:line in the result, never the matched text;
  - anything else is `ErrEnvironment`.
- **Binary content.** Any added or changed binary content fails the gate ("binary content not scanned").

**6. Gate weakening.**
- Driven by per-repo config: `tests.paths` (patterns), `tests.case_markers` and `tests.skip_markers` (RE2, compiled at load), and `tests.config_paths`.
- It fails when any of these hold:
  - a test file is deleted, or renamed out of the test paths;
  - case markers are removed more often than added;
  - an added line in a test file contains a skip marker;
  - anything under the config paths changes.
- A repo with no `tests.paths` fails validation.
- The JUnit report check in item 9 is the runtime half.

**7. Protected paths and execution surface.**
- Defaults are Go constants in `defaults.go`. Per-repo config adds to them and can never remove any.
- Both fill the existing `contracts.Constraints` fields (`internal/contracts/task.go:28-43`).
- **Matching:**
  - A pattern with no inner `/` matches a name at any depth.
  - A pattern with an inner `/` is anchored at the repository root.
  - A trailing `/` means a directory prefix.
  - `path.Match` applies within each component; `**` is refused.
  - Both sides are compared after `sandbox.FoldName` (`internal/sandbox/path.go:119`).
  - Renames and copies are checked on both the old and the new path.
  - A new symlink or gitlink (modes 120000 or 160000) fails as a protected path.
  - Any path that fails `sandbox.CheckPath` (`path.go:23`) fails too.

**8. Diff size.**
- `max_changed_lines` (fills `Constraints.MaxChangedLines`), `max_changed_files` and `max_diff_bytes`. All are required, and zero is invalid.
- An empty diff fails.

**9. Command gates and the JUnit report.**
- Build, lint and tests run in order through `Exec`, and stop at the first failure.
- Full output goes to the blob store, and its key is stored in `ArtifactKey`. Sandbox output never goes in `Detail`.
- **Tests:** the test command writes JUnit XML to `/tmp/conclave-junit/`. The gate deletes that directory before the run and reads it afterwards through `Exec cat`. The tests gate passes only when:
  - the exit code is 0;
  - the report parses;
  - more than zero tests ran;
  - none failed or errored.
- The report lives under `/tmp`, so nothing lands in the diff, and anything the agent planted there was wiped during staging.
- Each repo's test command must write the report. ETS adds `--reports-dir=/tmp/conclave-junit`.

**10. errored vs failed** (`classify.go`).
- **`ErrEnvironment`, so the attempt is `errored`:**
  - errors from the factory, `WriteFile`, staging or extraction;
  - a mismatched tree;
  - blob, store or scanner infrastructure failures;
  - `Exec` errors `ErrKilled`, `ErrClosed` and `ErrExpired`, and transport errors;
  - exit codes 126 and 127.
- **The attempt's own failure:** a non-zero exit, `TimedOut`, `ErrOutputTooLarge`, an apply or round-trip mismatch, or a failing static gate.
- `errored` writes no rows, so a retry starts clean. Ticket 8 maps a failed attempt to `AttemptFailed` with `ReasonGateFailed`, and an errored one to `AttemptErrored` with `ReasonSandboxError` (`internal/contracts/attempt.go:13-31`).

**11. API and persistence.**
- **`Gater.Gate(ctx, Input) (Outcome, error)`.**
  - If a complete result set for `(attempt, hash)` is already stored, it is returned without running.
  - Otherwise the gates run, and `Results.Record` inserts every row in one transaction with `ON CONFLICT (attempt_id, gate, artifact_hash) DO NOTHING`, then reads them back. The first writer wins (RL-2).
- **`gate.Verify(ctx, attemptID, hash)`** is ticket 9's single check before pushing. It re-hashes the blob through `blob.Get`, and requires that every required gate has a passing row.
- **`Input`:** `Artifact`, `Tier`, `Policy`, `Commands`, `InPlace contracts.Sandbox` (nil forces `clean_room`), `NewSandbox`, and `ForceCleanRoom`.
- **`Outcome`:** `Passed`, `Flagged`, `Mode`, and `Results`.
- The interfaces `Results` and `RejectionCounter` are defined in `gate`. `store` implements them using only `contracts` types.

**12. Mode.**
- `clean_room` if any of these hold:
  - `InPlace` is nil;
  - `ForceCleanRoom` is set;
  - the policy says so;
  - the tier is `sensitive` or `local_only`;
  - any static gate flagged (execution surface or `tool_calls`).
- Otherwise `reset_in_place`.
- An empty or unknown tier is an error. `SelectMode` is the only place the default lives, so Milestone 2 can set it from data.

**13. Scope.**
- **In:** build, lint, tests, diff_size, secrets, protected_paths, gate_weakening, execution_surface, and `tool_calls`, which flags when `RejectionCount` reaches `flag_rejections_at` (default 3) and never fails.
- **Deferred:**
  - `new_tests` (Milestone 2 test writer, FR-S7);
  - `dependencies` (no dependency tool yet; manifests are flagged as execution surface);
  - the FR-SEC24 pattern scan, which adds rules under `tool_calls` later;
  - the baseline run and FR-A4;
  - owner approval of protected-path changes, which fail in Milestone 1.

## Proposed contract changes (hand review)
```diff
--- internal/contracts/sandbox.go
 // disconnected context in deferred cleanup, or a cancelled workflow leaves the
 // container running (RL-3).
+//
+// No process outlives the call that started it: when Exec, ReadFile,
+// WriteFile or Diff returns, by any path, nothing it started still runs.
+// Freezing depends on this (FR-SEC27); a backend that cannot guarantee it
+// must not implement Sandbox.
 type Sandbox interface {
@@
+	// Diff output is produced inside the sandbox by tools its code can
+	// influence. Callers treat it as attacker-controlled input.
 	Diff(ctx context.Context) ([]byte, error)
--- internal/contracts/gate.go
 	GateExecSurface GateName = "execution_surface"
+
+	// GateToolCalls flags, and never fails, an attempt whose recorded tool
+	// calls are suspect: in Milestone 1, repeated path rejections
+	// (FR-SEC29). The FR-SEC24 pattern scan adds rules under the same name.
+	// A flag moves gating to clean_room (FR-SEC27).
+	GateToolCalls GateName = "tool_calls"
 )
@@ type GateResult struct {
-	// ArtifactHash ties this result to the exact diff that was checked.
+	// ArtifactHash ties this result to the exact artifact checked: the
+	// lowercase hex SHA-256 of the base commit and diff bytes as framed by
+	// gate.HashArtifact. The same diff on another base is another artifact.
 	ArtifactHash string `json:"artifact_hash"`
--- internal/contracts/record.go
+// ToolCallRecord (moved verbatim from internal/agent/record.go:14-47)
```
Also update the `Attempt.ArtifactHash` comment in `internal/contracts/attempt.go` to say "artifact (base commit plus diff)".

## Migration 00007 (`00007_gate_integrity.sql`)
- `REVOKE UPDATE ON gate_results FROM conclave_app`.
- A CHECK that `gate_results.artifact_hash ~ '^[0-9a-f]{64}$'`.
- A trigger that makes `attempts.artifact_hash` (`00001_work.sql:122`) write-once: NULL to a value is allowed, a value to anything else is refused.
- The down migration restores the grant and drops the CHECK and the trigger.

No `artifacts` table yet. The blob store holds the envelope, and ticket 9 adds lineage if a rebase needs it.

## Package and file layout (`internal/gate/`)
| File | Purpose |
|---|---|
| `doc.go` | Responsibility, step order, invariants: a gated sandbox is never handed back to an agent; the image needs sh, tar, git, env, find, mv, rm |
| `artifact.go` | `Artifact`, `HashArtifact`, `Freeze`, `Load` |
| `diff.go` | Strict, bounded parser for `git diff --binary` output (renames, copies, modes, binary sections, C-quoted paths) into `[]FileChange` |
| `match.go` | Folded path patterns |
| `policy.go` | `LoadConfig` for `deploy/gates.yaml` (KnownFields, a placeholder that fails), `Policy`, `Policy.Constraints()` |
| `defaults.go` | Protected-path and execution-surface floors |
| `size.go`, `protected.go`, `surface.go`, `weakening.go`, `toolcalls.go` | One static check each, as pure functions over `[]FileChange` |
| `secrets.go`, `gitleaks.go` | Scan input; the `SecretScanner` interface (an external-process boundary); the pinned runner |
| `mode.go` | `SelectMode` |
| `stage.go` | Wipe, re-stream, apply, round trip |
| `commands.go`, `junit.go` | Build, lint and tests; JUnit XML parsing |
| `classify.go` | `ErrEnvironment`, `ErrArtifactTooLarge`, `ErrUnstableDiff`, classification |
| `gate.go`, `verify.go` | `Gater`, `Input`, `Outcome`, `RequiredGates`, `newResult`, `Verify` |
| Tests | `*_test.go` beside each file, `attack_test.go` and `gate_docker_test.go` (Docker), `FuzzParseDiff`, `testdata/*.diff`. Fakes live in the package's own test files |

## Gate defaults
- **Protected paths:** `.github/`, `.gitlab-ci.yml`, `.circleci/`, `Jenkinsfile`, `.buildkite/`, `azure-pipelines.yml`, `.travis.yml`, `CODEOWNERS`, `.gitleaks*`.
- **Execution surface:**
  - package manifests and build files:
    - `package.json` (the whole file, which covers its scripts with no JSON diffing), lockfiles;
    - `pyproject.toml`, `setup.py`, `setup.cfg`;
    - `Cargo.toml`, `build.rs`, `go.mod`;
    - `pom.xml`, `.mvn/`, `mvnw`;
    - `build.gradle*`, `settings.gradle*`, `gradlew`, `gradle/wrapper/`;
    - `Gemfile`, `*.gemspec`;
  - hook installers: `.husky/`, `lefthook.y*ml`, `.pre-commit-config.yaml`, `.githooks/`;
  - registry config: `.npmrc`, `.yarnrc*`, `.pnpmfile.cjs`;
  - git: `.gitattributes` and `.gitmodules` at any depth;
  - build and environment files: `Makefile`, `*.mk`, `Dockerfile*`, `Containerfile`, `.devcontainer/`, `.vscode/`, `.idea/`, `.envrc`;
  - AI tooling: `.mcp.json`, `.claude/`, `.cursor/`, `CLAUDE.md`, `AGENTS.md`;
  - any +x mode change.
- **ETS values** (`deploy/local/gates.yaml`):
  - `tests.paths: [src/*Test.java]`;
  - `case_markers: ['@(Test|ParameterizedTest|RepeatedTest|TestFactory)\b']`;
  - `skip_markers: ['@Disabled', '@DisabledIf', 'assume(True|False|That)']`;
  - `config_paths: [lib/]`;
  - limits of 400 lines, 20 files and 256 KiB to start. The owner can change these.

## Order of one run
1. Load the artifact through `blob.Get`, which re-hashes it.
2. Run the static gates, all of them: diff_size, protected_paths, gate_weakening (static), execution_surface, `tool_calls`, secrets.
3. If any static gate failed, record the results and return failed, without touching a sandbox.
4. `SelectMode`.
5. `stage`.
6. Build, then lint, then tests (with the JUnit check). Stop at the first failure.
7. Record everything in one transaction.

`clean_room` sandboxes are closed in a deferred `Close(context.WithoutCancel(ctx))`.

## Threat table (each row has an attack test)
| Attack | Control | Test |
|---|---|---|
| A background writer survives "finish" | kill after every command; two identical `Diff` reads | `TestAttackFreezeBackgroundWriter` |
| The agent workspace is mutated after the freeze | no gate reads it; staging replaces it wholesale | `TestAttackPostFreezeMutationIgnored` (both modes) |
| skip-worktree, fsmonitor, `apply.whitespace=fix`, a planted ignored file, a `chmod 000` directory | `.git` and the workspace are wiped and re-streamed; git config isolated | `TestResetInPlaceIgnoresAgentGitState` |
| Textconv, clean filter or `git replace` makes `Diff` lie | the recorded bytes are what is applied, round-tripped and later pushed | `TestAttackTextconvDiffIsWhatIsGated` |
| The same diff reused on a different base | base bound into the envelope | `TestHashBindsBase` |
| `.GIT/…`, `git~1`, `..`, bidi characters, symlinks, gitlinks | `CheckPath` on both sides; 120000 and 160000 refused | `TestAttackArtifactPaths` (table) |
| A huge diff knocks over the worker or gitleaks | raw byte cap in `Freeze`; diff_size first | `TestFreezeRefusesOversizedDiff` |
| `System.exit(0)` in test code, or a planted JUnit report | the report must show more than zero tests; report directory deleted before the run; `/tmp` wiped during staging | `TestTestsExitZeroWithoutReportFails`, `TestAttackPlantedReportIgnored` |
| `gitleaks:allow`, a repo `.gitleaks.toml` or `.gitleaksignore`, a secret in a binary | `--ignore-gitleaks-allow`, stdin, empty working directory, platform config, binary fails, `.gitleaks*` protected | `TestAttackGitleaksAllowIgnored`, `TestAttackRepoGitleaksConfigIgnored`, `TestSecretsBinaryFails` |
| gitleaks missing, swapped or crashing | SHA-256 pin; exit code and report must agree | `TestSecretsMissing`, `…WrongHash`, `…ExitMismatch` (all errored) |
| A secret persisted through the evidence | `--redact`; allowlisted fields only | `TestSecretsEvidenceHasNoCanary` |
| A recorded row is rewritten | `REVOKE UPDATE`; hash CHECK; write-once attempt hash (run as `conclave_app`) | `TestGateResultsUpdateDenied`, `TestArtifactHashWriteOnce` |
| A retry flips the verdict | existing set returned; read-back | `TestGateTwiceRunsOnce` |
| A tampered blob on disk | `Get` re-hashes; key equals hash | `TestArtifactBlobTamperDetected` |

## Done-when traceability (all must exist and pass)
| Done-when item | Tests |
|---|---|
| Each gate has a fixture that fails it | `TestGates_EachGateFailingFixture` (a table with one `testdata/*.diff` per gate, plus build, lint and tests each exiting 1; `execution_surface` and `tool_calls` show *flagged*); `TestGitleaks_Real` (pinned binary; skipped locally, required in CI) |
| An attempt that deletes or skips a test is rejected | `TestGateWeakening/{deleted_test_file, renamed_out_of_tests, case_marker_removed, commented_out_test, skip_marker_added, test_config_changed}`; `TestGate_RejectsDeletedTest` |
| A diff touching `package.json` scripts, `.husky/`, `.gitattributes` or `.claude/` is flagged | `TestExecutionSurface_Flags` (nested, case-folded `.Claude/`, HFS-ignorable code points, quoted path, renamed in and out); `TestSelectMode_FlagForcesCleanRoom` |
| A gate result's artifact hash matches the diff that was checked | `TestGate_EveryResultCarriesCheckedHash` (the bytes the fake sandbox stages equal `blob.Get`); `TestStage_RoundTrip`; `TestAttackPostFreezeMutationIgnored` |
| An environment build failure yields `errored`, not `failed_gates` | `TestGate_EnvironmentFailuresAreErrored` (factory, upload, exit 127, `ErrKilled`, gitleaks missing or wrong hash; `ErrEnvironment` and no rows); `TestGate_BuildFailureIsFailed` |
| RL-2: safe to run twice | `TestGateTwiceRunsOnce`; `TestFreezeTwiceSameHash`; `TestGateResults_RecordIdempotentFirstWriterWins` |

## Execution
One session on `ticket/7`, following ticket 5's pattern:
1. **L0, the lead:** the contract batch (shown for hand review first), migration 00007, the `ToolCallRecord` move, and stubs for every owned file.
2. **Three workers in parallel worktrees:**
   - **W1:** `diff.go`, `match.go`, `policy.go`, `defaults.go` and the static gates.
   - **W2:** `artifact.go`, `stage.go`, `commands.go`, `junit.go`, `classify.go`, Docker tests.
   - **W3:** `secrets.go`, `gitleaks.go`, `store/gates.go`, `verify.go`.
3. **The lead:** `gate.go`, the end-to-end tests, the guide page.
4. `make check`, then `/review-ticket 7`.

Worktree agents start at `origin/main`, so reset each one to the L0 commit (lesson from ticket 6).

## Verification
- `make check` and `make check-ci` pass.
- The Docker gate tests run against `deploy/images/java21` and the busybox test image.
- The gitleaks real-binary test runs in CI with the pinned binary.
- One manual run: freeze and gate a hand-made ETS change in both modes. Check that every `gate_results` row carries the envelope hash, and that `Verify` passes, then fails after one byte of the blob is changed.

## Follow-ups owned by later tickets
- **Ticket 8:**
  - keep the prepared tree until gating finishes;
  - size `Limits.Timeout` to cover `reset_in_place` gating, or set `ForceCleanRoom` on retry;
  - map `Outcome` and errors onto attempt status and reason.
- **Ticket 9:** call `Verify` before pushing; re-gate after the finalization rebase (`Freeze` against the new base, then `Gate` with `InPlace=nil`).
- **Milestone 2:** the baseline run and FR-A4; hidden tests through an overlay step after the round trip (an additive `Input` field); `new_tests`; choosing the default mode from measured cost.
- **Milestone 3:** the dependencies gate; the FR-SEC24 pattern rules under `tool_calls`.
