# Ticket 8-pre — Workflow prerequisites

**Status:** merged as PR #19 (`94332eb`) · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-8-pre--workflow-prerequisites), plan [`ticket-8-pre-plan.md`](../../tickets/ticket-8-pre-plan.md) · **Decision:** [0011](../../decisions/0011-sandbox-release-by-id.md) (proposed) · **Requirements:** NFR-R2, NFR-R3, FR-SEC12, FR-SEC30, NFR-M7 · **Review list:** RL-2, RL-3, RL-13, RL-14

## In one paragraph

Ticket 8's workflow needs five things in its first steps, and before this ticket the merged code had none of them. It has to create the work rows (repository, feature request, plan, task, attempt), find a repository by its GitHub name, release its own sandbox by ID after a crash, know which image and limits each repository runs with, and tell the agent what the repository is. This ticket builds those five pieces and nothing else. It adds idempotent create functions in `internal/store`, `Release` by ID on the sandbox contract and the Docker provider, a validated `deploy/sandboxes.yaml`, a `profile` block in `deploy/toolchains.yaml`, and the actor strings the audit trigger records. There is no Temporal code.

## Why it exists

A review of tickets 1–7 and 8a found the gaps listed in the plan. `llm.Router.Scope` refuses to run until the work rows exist, and no code created them. The schema tracks `attempts.sandbox_id` and `sandbox_released_at` for cleanup, but nothing could write them, and nothing could remove a sandbox from its ID alone. `docker.Provider.Create` requires a digest-pinned image and limits that no configuration supplied. `contracts.RepoProfile` is rendered into every prompt and had no source.

The requirements shaped each piece:
- **NFR-R2 and RL-2** (idempotent actions): every create can run twice and still leave one row.
- **NFR-R3 and RL-3** (guaranteed cleanup): this is why `Release` exists and why the store records both the sandbox ID and the release.
- **FR-SEC12** (no tier, no run): `EnsureRepo` makes the database tier follow configuration.
- **FR-SEC30** (images by digest): `sandboxes.yaml` refuses an image by tag.
- **NFR-M7** (validated plain configuration): both YAML loaders fail at startup with the offending key in the message.
- **RL-13** (compare-and-set): `FinishAttempt` goes through `Transition`.
- **RL-14** (Temporal versus Postgres): the actor string carries the workflow ID into the audit table.

## How it works

**Contract batch** (`internal/contracts`, reviewed by hand). `SandboxProvider` gains `CreateWithInstall`, lifted from the Docker provider so the install phase is reachable through the interface, and `Release(ctx, id)`. `FailureReason` gains `ReasonCancelled` (an owner cancellation mid-attempt is a reason on an errored attempt, not a fifth state) and `ReasonBudgetCap` (a feature-request or monthly cap refused the call).

**Releasing a sandbox by ID** (`docker.Provider.Release`):
1. Release inspects the container. If it isn't found, that's success.
2. A container without the `conclave.sandbox=true` label is refused with `ErrNotSandbox` and left alone. An empty ID is refused the same way, so a caller bug isn't mistaken for "nothing to do".
3. Otherwise it force-removes the container through `removeContainer`, the same helper `Sandbox.Close` uses.
4. It then looks up the network **by the sandbox's name**. It doesn't use the container's endpoints, because the install phase has already disconnected them.
5. It removes the network only when the network carries the sandbox label and the container's namespace.

Release ignores the provider's own namespace, because the ID comes from an attempt row that another worker may have written. If the container is removed but the network removal fails, a retry finds no container and returns nil, and `Reap`'s age sweep removes the network later.

**Work rows** (`internal/store/work.go`, `work_attempt.go`). Each create is `INSERT … SELECT … WHERE EXISTS (parent) ON CONFLICT (natural key) DO NOTHING RETURNING`, then a `SELECT` on a miss. A second call returns the existing row with `created == false` and never overwrites it.

Inputs are checked before any SQL runs:
- UUIDs through `checkUUID`;
- text fields for NUL bytes and invalid UTF-8;
- JSON for validity, and for `\u0000`, which jsonb refuses.

Without these checks, a refusal from Postgres would abort the caller's transaction. A missing parent is `ErrNotFound` for the same reason, rather than a foreign-key violation. Like `Transition`, the creates need READ COMMITTED: on a conflict they read the existing row back in a new statement, and only that level lets it see a row another transaction has just committed.

The functions:

| Function | Natural key | Notable behaviour |
|---|---|---|
| `EnsureRepo` | `full_name` | Upsert. Tier and default branch are overwritten from configuration. The input ID is ignored. |
| `RepoDirectory.RepoByFullName` | `full_name` | Mirrors `Repo`. |
| `CreateFeatureRequest` | `(repo_id, issue_number)` | Starts in `planning`. A different `workflow_id` on the existing row is `ErrWorkflowMismatch`. |
| `CreatePlan` | `(feature_request_id, version)` | Starts in `proposed`. The body is opaque JSON, so the returned `Plan.Tasks` is empty. |
| `CreateTask` | `(plan_id, key)` | Starts in `running`, with the spec stored as jsonb. |
| `CreateAttempt` | `(task_id, ordinal)` | Starts in `running`. All four provenance fields are required. |

The create functions take no actor. The audit trigger fires on `UPDATE OF state` only, so an insert writes no audit row, and the trail starts at the first transition.

Attempt bookkeeping:
- **`SetAttemptSandbox`** is write-once. Ticket 8 calls it straight after `Create`, because the Docker provider chooses the container's name. The same value again is a no-op, and a different one is `ErrSandboxMismatch`.
- **`MarkSandboxReleased`** stamps `now()` once.
- **`SetArtifactHash`** accepts only lowercase hex SHA-256 and is write-once in the same way (`ErrArtifactMismatch`). The 00007 trigger still refuses a direct UPDATE behind it.
- **`FinishAttempt`** runs `Transition` from `running` first. It writes usage, model, provider, report and `finished_at` only when that transition succeeded, conditional on the new state and revision. A retry gets `ErrAlreadyApplied` and writes nothing. A failed or errored attempt must carry a reason, and a passed one must not.
- **`UnreleasedAttempts(ctx, db, olderThan)`** returns `UnreleasedAttempt{Attempt, SandboxID}`, oldest first, measured on the database clock. The sandbox ID is carried beside the contract record, which has no field for it.

**Actors** (`internal/store/actor.go`):
- `ActorWorkflow(id)` gives `workflow:<id>`;
- `ActorGitHub(userID)` gives `github:<numeric id>`;
- `ActorWorker` is `worker`, for startup seeding and the reaper.

**Sandbox configuration** (`internal/sandbox/config.go`). `sandbox.LoadConfig` reads `deploy/sandboxes.yaml` into a `RepoSandbox` per repository. Each entry holds the default branch, image, limits, command timeout, install steps and env. It applies the provider's own rules, which now live in `internal/sandbox/spec.go` (`DigestPinned`, `ValidEnvKey`, `ReservedEnv`, `CheckLimits`) so the Docker provider and the loader share one source.

On top of those rules it refuses:
- the placeholder `owner/repo` and the all-zero digest;
- sizes that aren't a whole number of B, KiB, MiB or GiB;
- a sandbox timeout that isn't longer than the command timeout;
- an empty install step;
- a default branch git would refuse or misread: whitespace, a control character, a leading `-` (git would take it for a flag), `..`, or a `.lock` suffix. The branch is stored trimmed.

The real entry lives in git-ignored `deploy/local/sandboxes.yaml`.

**Repository profile** (`internal/agent/toolchains.go`). `agent.LoadToolchains` now returns `map[string]Toolchain`, each holding `Commands` and a `contracts.RepoProfile`. The profile block sits beside `test`/`build`/`lint`. It needs `language`, `layout_hint` and `conventions`, each non-blank and trimmed. `FullName` comes from the key.

## Where to look

| File | What's in it |
|---|---|
| `internal/contracts/sandbox.go`, `attempt.go` | `CreateWithInstall`, `Release`, `ReasonCancelled`, `ReasonBudgetCap` |
| `internal/sandbox/docker/provider.go` | `Release`, `releaseNetwork`, `ErrNotSandbox` |
| `internal/sandbox/docker/sandbox.go` | `removeContainer`, `removeNetwork`, shared by `Close` and `Release` |
| `internal/sandbox/spec.go` | Digest, env and limit rules shared by the provider and the loader |
| `internal/sandbox/config.go` | `LoadConfig`, `RepoSandbox`, `parseSize` |
| `deploy/sandboxes.yaml` | Placeholder sandbox configuration, documented field by field |
| `internal/store/work.go` | Repo, feature request, plan and task creates; `RepoByFullName` |
| `internal/store/work_attempt.go` | Attempt create, sandbox and hash writes, `FinishAttempt`, `UnreleasedAttempts` |
| `internal/store/actor.go` | Actor strings for the audit trigger |
| `internal/agent/toolchains.go`, `deploy/toolchains.yaml` | `Toolchain`, the `profile` block |

## What the tests prove

- **Release removes the container and its network.** `TestReleaseRemovesContainerAndNetwork` covers both a plain sandbox and a networked `CreateWithInstall` sandbox.
- **Release is idempotent.** `TestReleaseTwiceReturnsNil`, `TestReleaseOfUnknownIDReturnsNil` and `TestCloseAfterReleaseReturnsNil`.
- **Release never removes what the provider didn't create.** `TestReleaseRefusesUnlabelledContainer` (the container survives), `TestReleaseLeavesUnlabelledNetwork` and `TestReleaseRefusesEmptyID`.
- **Every create is idempotent and never overwrites.** `TestEnsureRepo`, `TestCreateFeatureRequest`, `TestCreatePlan`, `TestCreateTask` and `TestCreateAttempt` each run their function twice in one application-role transaction and assert one row and the first call's values. The four creates on state tables also assert no audit rows (`repos` has no state column, so `EnsureRepo` can't write one). They also cover the `ErrInvalidID`, `ErrNotFound` and `ErrWorkflowMismatch` refusals without aborting the transaction.
- **Configuration decides the tier, and the rows satisfy the router.** In `TestWorkRowsOpenRouterScope`, a row seeded as `sensitive` becomes `public` through `EnsureRepo`. `llm.Router.Scope` then succeeds on rows the new functions created, and a call is charged to that attempt.
- **Sandbox and hash writes are write-once.** `TestSetAttemptSandbox` (`ErrSandboxMismatch`), `TestMarkSandboxReleased` and `TestSetArtifactHash`. The last checks both `ErrArtifactMismatch` and that the trigger still refuses a direct UPDATE.
- **Finishing twice changes nothing.** `TestFinishAttempt` checks a retry gets `ErrAlreadyApplied`, there is one audit row, and the usage is the first call's.
- **The reaper's query is exact.** `TestUnreleasedAttempts` returns the old unreleased row only. It excludes released rows, recent rows and rows with no sandbox recorded.
- **The application role can't delete work rows.** `TestAppRoleCannotDeleteWorkRows`.
- **Actor strings have a fixed format.** `TestActorStrings`.
- **The sandbox loader refuses bad configuration.** `TestLoadConfigRejects` covers the placeholder, the zero digest, an image by tag, zero limits, bad sizes, a reserved env name, a timeout not longer than the command timeout, unknown fields and more. `TestLoadConfigAcceptsAValidFile` checks the parsed bytes and durations, and `TestLoadConfigTrimsDefaultBranch` checks the branch reaches git without surrounding whitespace. `TestLoadConfigShippedFile` checks the shipped file fails until both placeholders are replaced. `TestParseSize` covers the size parser.
- **The profile is required.** `TestLoadToolchains` fails on a missing profile block, each blank field, and an unknown profile key, and returns the trimmed profile with `FullName` set.

## Left open

- **Nothing calls these functions yet.** Ticket 8 wires them into the workflow, its activities and `cmd/worker`. That includes the check that `repos.yaml`, `toolchains.yaml`, `gates.yaml` and `sandboxes.yaml` name exactly the same repositories.
- **There is a window between creating a sandbox and recording its ID**, because the provider names the container. If a crash lands there, only `Reap`'s age sweep removes that sandbox ([0011](../../decisions/0011-sandbox-release-by-id.md)).
- **A failed network removal after a successful container removal** is also left for `Reap`.
- **`UnreleasedAttempts` returns running attempts too.** Ticket 10's reaper must pass an age above the longest attempt, or skip running rows itself.
- **The sandbox image comes from a local build** for the Milestone 1 trials. The CI build and GHCR publication from FR-SEC30 are deferred ([0011](../../decisions/0011-sandbox-release-by-id.md)).
- **The all-zero digest is refused only by `LoadConfig`.** The Docker provider's `validateSpec` still accepts it, because its own tests use it.

## Check your understanding

1. Why does `Release` find the network by name rather than through the container's network settings?
2. A retried `CreateFeatureRequest` arrives with a different workflow ID. What happens, and why is that an error rather than an update?
3. Why does `FinishAttempt` run the transition before writing usage, and what does a retry after a successful commit see?
4. Why do the create functions check UUIDs, text and JSON in Go before sending SQL, when Postgres would refuse the same values?
5. What is the gap between `Create` and `SetAttemptSandbox`, and what cleans up a sandbox lost in it?
6. Why is a cancelled attempt `errored` with reason `cancelled`, and not a new state?
