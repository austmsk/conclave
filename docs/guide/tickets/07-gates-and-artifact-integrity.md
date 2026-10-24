# Ticket 7 — Gates and artifact integrity

**Status:** in review on `ticket/7` · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-7--gates-and-artifact-integrity) · **Plan:** [`ticket-7-plan.md`](../../tickets/ticket-7-plan.md) · **Requirements:** FR-A1, FR-A3, FR-SEC25, FR-SEC27 (also FR-SEC8, FR-SEC24, FR-SEC29, FR-I4) · **Review list:** RL-2

## In one paragraph

`internal/gate` decides whether an agent's change may become a pull request. When the agent finishes, `Freeze` turns its diff into an **artifact**: an envelope that binds the diff bytes to the base commit, stored in the blob store under its own SHA-256. `Gater.Gate` then checks that artifact, never the agent's live workspace. It first runs six static gates over the parsed diff: diff size, protected paths, gate weakening, execution surface, `tool_calls` and secrets. If they pass, it rebuilds base plus diff in a sandbox it has wiped and re-filled from the worker's clean copy of the tree, and runs build, lint and tests there. Every result is stored with the artifact's hash, and `Verify` is the single check ticket 9 makes before pushing. Ticket 7 also adds migration 00007, which makes those rows tamper-resistant; `store.GateResults`; `deploy/gates.yaml`; and a platform `deploy/gitleaks.toml`.

## Why it exists

The gates are the only thing between a model's output and the owner's repository, so they have to answer two questions honestly. Does the change pass, and is the change that passed the one that gets pushed?

- **What was checked is what gets pushed** (FR-SEC27, FR-A3).
  - The hash covers the base and the diff (`HashArtifact`), so the same diff rebased onto a new base is a new artifact that must be gated again.
  - `newResult` is the only constructor of a `GateResult`, and it copies the hash from the artifact.
  - Migration 00007 revokes `UPDATE` on `gate_results` from `conclave_app`, checks the hash's shape, and makes `attempts.artifact_hash` write-once.
- **The agent controls its workspace, including `.git`.** A process left running, a `.git/config` with a textconv driver, skip-worktree bits or a planted JUnit report could all make a live check lie. So nothing after `Freeze` reads that workspace:
  - staging wipes it and streams in the prepared tree from the worker;
  - it applies the recorded bytes with git configuration isolated;
  - it re-diffs and requires byte equality.
- **A test suite can be made to pass by removing it** (FR-A1). The gate-weakening gate fails these:
  - a deleted test file, or one renamed out of the test paths;
  - fewer test-case markers than before;
  - a new skip marker;
  - a moved block-comment boundary in an existing test file: a run of adjacent added or removed lines that leaves a comment open, or closes one opened elsewhere (`tests.comment_openers` and `tests.comment_closers`). Wrapping tests in `/* */` leaves unchanged lines between the opener and the closer, so it fails; adding or removing a whole Javadoc block does not. New test files are not checked, since they have no existing cases to hide;
  - any change to test configuration.

  At run time, the tests gate also requires a JUnit report showing at least one test ran and none failed.
- **Some files run code, or reconfigure tools, outside the test run** (FR-SEC25). Examples are package manifests, hook installers, `.gitattributes` and `.claude/`. A change to one is *flagged*, which moves gating to `clean_room`. Protected paths such as CI configuration, `CODEOWNERS` and `.gitleaks*` fail outright (FR-SEC8).
- **A broken environment is not the agent's fault.** A missing binary, a lost Docker connection or a store outage returns `ErrEnvironment`, so the attempt is `errored` rather than `failed`. Nothing is recorded, and a retry starts clean.
- **RL-2: safe to run twice.** A retried `Gate` that finds a complete result set returns it without running anything. `Record` inserts with `ON CONFLICT DO NOTHING` and reads the rows back, so the first writer wins and a retry cannot flip a verdict.

## How it works

**1. Freeze** (`artifact.go`).
- `Freeze(ctx, sb, blobs, repoID, base)` calls `sb.Diff` twice and requires identical bytes; otherwise it returns `ErrUnstableDiff`.
- This works because the `Sandbox` contract now guarantees that no process outlives the call that started it.
- A diff over `MaxArtifactBytes` returns `ErrArtifactTooLarge`.
- It stores `Envelope(base, diff)`, which is `conclave-artifact v1\nbase <sha>\n\n<diff>`, and returns an `Artifact{RepoID, Base, Hash, Key, Size}`.
- `Load` reads the artifact back through `blob.Get`, which re-hashes it, and then checks the envelope and its hash itself, so a store that does not verify on read is covered too. A missing, corrupt or unreachable blob is `ErrEnvironment`: the store failed, not the attempt.

**2. Gate** (`gate.go`: `Gater.Gate`).
1. `checkInput` refuses a caller bug (no attempt, artifact, tree, factory or commands) as a plain error.
2. `recorded`: if every gate in `RequiredGates` already has a row for `(attempt, hash)`, return those rows.
3. `Load` the artifact, and check it is the one the `Input` names.
4. `runStatic` parses the diff with `ParseDiff`, a strict, bounded parser for `git diff --binary`. A diff it refuses fails every static gate. It then runs:
   - `checkDiffSize`;
   - `checkProtected`;
   - `checkWeakening`;
   - `checkExecSurface`;
   - `checkToolCalls` over `RejectionCount`;
   - `checkSecrets`, skipped if the size gate failed.
5. `SelectMode` picks `clean_room` if any of these hold, and `reset_in_place` otherwise:
   - there is no agent sandbox;
   - the caller forces it;
   - the policy says so;
   - the tier is `sensitive` or `local_only`;
   - a static gate flagged.
6. If a static gate failed, build, lint and tests are recorded as "not run: a static gate failed", and no sandbox is touched.
7. Otherwise `runInSandbox` stages and runs the command gates. A `clean_room` sandbox comes from `Input.NewSandbox` and is closed on every path with `context.WithoutCancel`.
8. `record` builds every row with `newResult`, and `Results.Record` stores them in one transaction.

**3. Staging** (`stage.go`: `stage`). This is the same for both modes.
1. `archiveTree` checks the worker's prepared tree with `sandbox.VerifyTree` and requires `HeadCommit == Artifact.Base`. It then tars the tree with `sandbox.WriteTree`, moved from the docker package for this ticket.
2. `wipe` runs `chmod -R u+rwX`, empties `/workspace` (`.git` included) and `/tmp`, and checks both are empty. `/home/agent` is kept (accepted threat T8a).
3. `unpackTree` uploads the tar, moves it to `/tmp` and extracts it.
4. `applyDiff` uploads the recorded diff and runs `git apply --binary` under `env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null HOME=/nonexistent`.
5. `roundTrip` runs the command `Diff` uses, plus `--no-textconv`, and compares the bytes.

A diff that does not apply or does not round-trip is the attempt's own failure: build, lint and tests are recorded as "not run: artifact does not reproduce on base".

**4. Command gates** (`commands.go`, `junit.go`).
- Build, lint and tests run in order, and stop at the first failure.
- Each command's full output goes to the blob store, keyed in `ArtifactKey`. `Detail` holds only platform-written text such as `exit 1`.
- Before the tests run, `JUnitDir` (`/tmp/conclave-junit`) is deleted. Afterwards, `find … -exec cat` reads every regular `*.xml` file in it, and `judgeReport` requires that more than zero tests ran and that none failed or errored.

**5. Secrets** (`secrets.go`, `gitleaks.go`).
- The scan input is the added lines only, under a header naming each path. Any binary change fails the gate as "binary content not scanned".
- `Gitleaks` checks the pinned SHA-256 at startup and before every scan, and runs from a private copy with an empty environment, an empty working directory and the platform config. Its flags are `--redact`, `--ignore-gitleaks-allow`, `--exit-code 99`, and `--gitleaks-ignore-path` set to that empty directory, so no `.gitleaksignore` anywhere can apply.
- A result keeps only the rule ID, the path and the line number in the new file, which `ParseDiff` reads from the hunk headers (`FileChange.AddedAt`; `RemovedAt` numbers removed lines in the old file). The parser refuses hunks git never writes, such as a side with lines starting at line 0, or hunks out of order, so line numbers are always from 1 and strictly increasing.
- An exit code that disagrees with the report is `ErrEnvironment`.

**6. Errored or failed** (`classify.go`).
- `classifyExec` and `stepError` sort every sandbox outcome:
  - a non-zero exit, a timeout or `ErrOutputTooLarge` from a gate command fails that gate;
  - exit 126 or 127, any other `Exec` error, and any failed platform step are `ErrEnvironment`;
  - the caller's own cancellation is reported as such.

**7. Persistence and Verify** (`internal/store/gates.go`, `verify.go`).
- `store.GateResults` implements `gate.Results` with `ON CONFLICT (attempt_id, gate, artifact_hash) DO NOTHING`, and takes a `QueryTxDB`: the existing `TxDB` opens transactions but, like `DBTX`, has no `Query` for reading several rows back.
- `Verify(ctx, blobs, results, attemptID, repoID, hash)` reloads the artifact, re-hashing it, and requires a passing row for every required gate under that hash. It returns `ErrNotVerified` naming each missing or failing gate. The caller names the artifact by hash, and must take that hash from the attempt's own record; after a rebase, one attempt has more than one fully gated artifact.

**Configuration** (`policy.go`, `defaults.go`, `deploy/gates.yaml`).
- `LoadConfig` reads the gitleaks pin and a `Policy` per repository with unknown keys refused. The checked-in file is a placeholder that fails validation.
- Protected-path and execution-surface defaults are constants. A repository can add to them, never remove.
- Patterns are matched after `sandbox.FoldName`:
  - a name with no inner `/` matches at any depth;
  - one with an inner `/` is anchored at the root;
  - a trailing `/` means a directory.
- `Policy.Constraints()` fills the existing `contracts.Constraints` fields.

## Where to look

| File | What's in it |
|---|---|
| `internal/gate/artifact.go` | `Artifact`, `Envelope`, `HashArtifact`, `Freeze`, `Load` |
| `internal/gate/gate.go` | `Gater`, `Input`, `Outcome`, `RequiredGates`, `newResult`, the run order |
| `internal/gate/stage.go` | Wipe, re-stream, isolated apply, round trip |
| `internal/gate/commands.go`, `junit.go` | Build, lint, tests; JUnit parsing and judgement |
| `internal/gate/diff.go` | `ParseDiff` into `[]FileChange` |
| `internal/gate/size.go`, `protected.go`, `weakening.go`, `surface.go`, `toolcalls.go` | One static gate each |
| `internal/gate/secrets.go`, `gitleaks.go` | Scan input, `SecretScanner`, the pinned runner |
| `internal/gate/match.go`, `policy.go`, `defaults.go`, `mode.go` | Patterns, configuration, built-in floors, `SelectMode` |
| `internal/gate/classify.go` | `ErrEnvironment`, `ErrArtifactTooLarge`, `ErrUnstableDiff`, `classifyExec` |
| `internal/gate/verify.go` | `Verify`, `ErrNotVerified` |
| `internal/store/gates.go` | `GateResults`, `QueryTxDB` |
| `internal/store/migrations/00007_gate_integrity.sql` | `REVOKE UPDATE`, hash CHECK, write-once `attempts.artifact_hash` |
| `internal/contracts/sandbox.go`, `gate.go`, `record.go` | No-process-outlives-the-call guarantee, `GateToolCalls`, `ToolCallRecord` (moved from `agent`) |
| `internal/sandbox/archive.go` | `WriteTree`, moved from `internal/sandbox/docker` |
| `deploy/gates.yaml`, `deploy/gitleaks.toml` | Placeholder policy; platform gitleaks config with a `canary_*` rule |
| `.github/workflows/ci.yml` | Installs gitleaks v8.30.1, checking the tarball and binary SHA-256 |
| `.gitleaks.toml` | Config for scanning Conclave's own repository: allowlists only `canary_*` fixture values. Never used by the gate |
| `deploy/toolchains.yaml` | Documents that each test command must write a JUnit report to `/tmp/conclave-junit/` |

## What the tests prove

**Each gate has an artifact that fails it.**
- `TestGates_EachGateFailingFixture` runs the whole of `Gate`, one case per gate. `execution_surface` and `tool_calls` flag and move gating to `clean_room`.
- `TestStaticGates_Fixtures`, `TestGitleaks_Real` (pinned binary; skipped locally, required in CI).

**Deleting or skipping a test is rejected.**
- `TestGateWeakening` (deleted, renamed out, marker removed, commented out with `//` and with `/* */`, an opener left unclosed or beside a dummy opener, a comment closer removed, Javadoc added or removed whole, a new test file, skip marker, config changed), `TestGate_RejectsDeletedTest`.
- `TestTestsExitZeroWithoutReportFails`, `TestAttackPlantedReportIgnored`.

**Execution surface is flagged.**
- `TestExecutionSurface_Flags` (nested, case-folded `.Claude/`, ignorable code points, quoted, renamed in and out), `TestSelectMode_FlagForcesCleanRoom`.

**The hash on every result is the artifact that was checked.**
- `TestGate_EveryResultCarriesCheckedHash`: in both modes, every row carries the hash, and the bytes staged are the blob's.
- `TestHashBindsBase`, `TestStage_RoundTrip`, `TestAttackPostFreezeMutationIgnored`, `TestAttackTextconvDiffIsWhatIsGated`, `TestResetInPlaceIgnoresAgentGitState`, `TestAttackFreezeBackgroundWriter`.
- `TestArtifactBlobTamperDetected`, `TestLoadCorruptEnvelope`, `TestVerify`, `TestVerifyTamperedBlob`, `TestVerifyMissingBlob`.
- `TestGateResultsUpdateDenied`, `TestGateResultsHashCheck`, `TestArtifactHashWriteOnce` (as `conclave_app`).

**An environment failure is errored, not failed.**
- `TestGate_EnvironmentFailuresAreErrored`: factory, upload, exit 127, `docker.ErrKilled`, an unprepared tree, gitleaks missing or swapped, a missing artifact blob, and store failures each return `ErrEnvironment` with no rows.
- `TestGate_BuildFailureIsFailed`, `TestClassifyExec`, `TestSecretsMissing`, `TestSecretsWrongHash`, `TestSecretsExitMismatch`.

**RL-2: safe to run twice.**
- `TestGateTwiceRunsOnce`: a second `Gate` returns the first, failing verdict without running, even when the sandbox would now pass.
- `TestGate_PartialRecordRunsAgain`, `TestFreezeTwiceSameHash`, `TestGateResults_RecordIdempotentFirstWriterWins`.

**Other guarantees.**
- `TestGate_StaticFailureSkipsSandbox`, `TestGate_NotReproducedFails`, `TestGate_CleanRoomClosed`, `TestGate_RefusesBadInput`.
- `TestAttackArtifactPaths` (`.GIT`, `git~1`, `..`, bidi, symlinks, gitlinks).
- `TestAttackGitleaksAllowIgnored`, `TestAttackRepoGitleaksConfigIgnored`, `TestSecretsEvidenceHasNoCanary`, `TestSecretsBinaryFails`.
- `TestFreezeRefusesOversizedDiff`, `FuzzParseDiff` (which also checks that every changed line has a line number), `TestCheckSecretsRefusesUnlocatedChange`.

## Left open

- **Decision records are owed by the owner:** artifact identity (the envelope and re-streamed staging); gitleaks on the worker; error classification without a baseline.
- **Known limits:**
  - Comment openers and closers are per repository, optional and syntax-based, and read one line at a time. ETS's match `/*` and `*/` even inside strings, so in an existing test file, adding a line such as `String g = "src/*";`, or changing only the closing line of a Javadoc, fails the gate.
  - Removing an existing line such as `foo /* bar */` from inside a block comment extends that comment, and neither pattern catches it. An agent can only exploit such a line if the repository already has one.
  - Any other way of silencing a test that leaves the case markers, skip markers and comment boundaries alone, while one test still runs, waits for the FR-A4 baseline in Milestone 2.
  - Staging extracts the tree through `/tmp`, an eighth of the sandbox's disk on Docker, so a very large repository fails staging as `ErrEnvironment`.
  - Test code, or an agent-edited Makefile, can `exit 127` to turn a failed attempt into an errored one. No rows are written, so nothing passes; FR-SEC27 bounds it to one retry before escalation.
- **Deferred:**
  - `new_tests` (Milestone 2);
  - `dependencies` (Milestone 3);
  - the FR-SEC24 pattern scan under `tool_calls`;
  - the baseline run and FR-A4;
  - owner approval of protected-path changes, which fail outright in Milestone 1.
- **Ticket 8:**
  - keep the prepared tree until `Gate` returns;
  - size `Limits.Timeout` for `reset_in_place` gating;
  - map `Outcome` and `ErrEnvironment` to `AttemptFailed`/`ReasonGateFailed` and `AttemptErrored`/`ReasonSandboxError`.
- **Ticket 9:**
  - call `Verify` before every push, with the hash from the attempt's record;
  - after the finalization rebase, `Freeze` against the new base and `Gate` with `InPlace` nil;
  - decide where the rebased artifact's hash lives. Migration 00007 makes `attempts.artifact_hash` write-once, so it stays the first frozen artifact's hash, and `UPDATE attempts SET artifact_hash = <new>` fails. Ticket 9 needs either a separate column for the pushed artifact or an artifacts table recording the lineage.

## Check your understanding

1. Why does the artifact hash include the base commit, and what has to happen when ticket 9 rebases a diff?
2. The agent edits `.git/config` to add a textconv driver. Why does that not change what is gated or pushed?
3. A test command calls `System.exit(0)` before any test runs. Which check fails it?
4. What makes the difference between an `errored` and a `failed` attempt, and why does an errored one write no rows?
5. `Gate` is retried after `Record` committed but before the reply arrived. What does the retry do?
6. Which changes are flagged rather than failed, and what does a flag change about how the artifact is gated?
