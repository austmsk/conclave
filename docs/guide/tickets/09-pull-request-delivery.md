# Ticket 9 — Pull request delivery

**Status:** in review on branch `ticket/9` · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-9--pull-request-delivery) · **Requirements:** FR-S10, FR-S16, FR-S17, FR-S19, FR-A5, FR-A7, FR-SEC27 · **Review list:** RL-2, RL-21 · **Decision:** [0014](../../decisions/0014-pull-request-delivery.md)

## In one paragraph

A passed task now becomes a pull request. The `pr_writer` model writes a title, summary, risks and a Conventional Commits subject. Platform code builds one commit from the gated artifact, authored by the App's bot, and pushes it to `agent/<issue>/integration`. It then rebases the diff once onto the default branch's current head, inside a fresh sandbox, and gates the rebased artifact again in `clean_room`. The rebased commit goes to `agent/<issue>/pr-1` and opens as a **draft** pull request whose body the platform renders. The workflow polls the repository's CI check runs on a timer. When every required check passes, the draft is marked ready and the feature request moves `executing → delivered`. Before this ticket no run could reach `delivered`. Every step is recorded in a new `pull_requests` row, written ahead of each outside action, so a Temporal retry or an owner's `/retry` resumes without a second model call, push or pull request.

## Why it exists

Ticket 8's `Deliver` was a stub that verified the artifact and stopped in `needs_attention`. The spec asks for the rest of the path: a described PR (FR-S10), the two-branch model (FR-S16), bot-authored squashed commits with trailers (FR-S17), one rebase and full gate rerun at finalization (FR-S19, FR-A5), ready only when real CI passes (FR-A7), and a push path that refuses any artifact without a passing gate result (FR-SEC27). RL-2 shaped every activity: each can run twice, and the pull request and commits are deterministic so the second run finds the first's work. RL-21 is mostly Milestone 4's, but its first step lands here: each PR's base SHA is recorded write-once at branch creation, so restacking needs no migration later.

## How it works

The workflow side is `internal/workflow/delivery.go`. `FeatureWorkflow` reads `GetVersion("delivery-9")` once into `featureRun.v9`. A run started before this ticket reads the default version and keeps the stub (`TestDeliveryBeforeTicket9KeepsStub`). A new run calls `deliverPR`, which runs these activities on one `activity.DeliveryInput` (attempt, task, artifact hash, pinned base, repository, issue):

1. **`WritePullRequest`** (`internal/activity/delivery.go`). If a `pull_requests` row exists for the attempt, it returns at once. Otherwise it checks the attempt passed with this artifact on this base and that `gate.Verify` accepts it. It then calls the `pr_writer` through `Router.FeatureRequestScope`, so the feature request and monthly caps bound it, with no attempt budget. `callPRWriter` takes the role's spec from `Deps.Roles`, like the attempt activity. A worker without one refuses permanently, before any model call. The spec's limits bound the calls: at most `MaxSteps`, a repair only while the tokens used so far are under `MaxTokens`, and all within `MaxWallClock`. The prompt and parser are in `internal/agent/prwriter.go`: `PRWriterRequest`, `ParsePRWriterOutput`, and `PRWriterRepair` for the one repair call. The diff, the completion report and the issue text go in the user message under "untrusted" labels, never in the system prompt. The parser accepts exactly one JSON object and validates the commit type and scope, a description of at most 72 bytes on one line, and field sizes. It drops every trailer-shaped line from the body. In every field it rewrites the reference after a GitHub closing keyword as plain words, so `Fixes #12` becomes `Fixes issue 12`. Merging the PR, or squash-merging with its title as the subject, can close only the issue the platform names in `Closes #<issue>`. `commitMessage` (`delivery_parts.go`) appends the trailers `Task: <task id>`, `Plan: <plan id> v<version>`, `Model: <attempts.model>` and `Generated-by: conclave`. `store.CreatePullRequest` records it all with the branch names `agent/<n>/integration` and `agent/<n>/pr-1`. A budget refusal returns `Stop: budget_cap`. Two invalid replies are a permanent error.
2. **`Land`**. `buildCommit` verifies the original artifact, prepares a tree at the pinned base through `TreeSource`, and calls `sandbox.CommitDiff` (`internal/sandbox/commit.go`). That builds the commit on the host with a temporary index: `read-tree`, `apply --cached --binary`, `write-tree`, `commit-tree`. The work tree and refs are never written, and git runs with a from-scratch environment. The author and committer are `Deps.Bot` (`github.Config.BotIdentity`: `<slug>[bot]` and `<bot_user_id>+<slug>[bot]@users.noreply.github.com`), and both dates are `attempts.finished_at`. Since the message is stored, the commit SHA is the same every time. `Activities.push` pushes it to the integration branch with lease `""`, then `landed_commit` is recorded.
3. **`Finalize`** runs on the attempt queue (one slot, heartbeat, two tries for FR-SEC27). `newBase` clones the default branch, reads its head and records it as `base_sha`; a retry reuses the recorded base. If the base hasn't moved, the original artifact and its gate results stand. Otherwise `rebased` creates a sandbox with no network over the new-base tree and calls `gate.Rebase` (`internal/gate/rebase.go`). That uploads the diff, refreshes the index, runs `git apply --3way --binary` under isolated git config, and `Freeze`s the result against the new base. The bytes come from the same git the gate's round trip uses. A refused apply is `ErrRebaseConflict`, which becomes `Stop: rebase_conflict`. The rebased hash is recorded, then `gateRebased` calls `gate.Gate` unchanged, with `InPlace` nil, so it runs in `clean_room`. A failing verdict becomes `Stop: rebase_gate_failed`.
4. **`OpenPullRequest`**. `buildCommit` runs again on the rebased artifact at the new base, which re-verifies it. The head commit is recorded first. Then integration is pushed with the landed commit as its lease, and `pr-1` with lease `""`. `CreatePullRequest` opens a draft from `pr-1` to the default branch. `renderPullRequestBody` writes, in order: the AI-generated note, a "Security-relevant changes" section listing every flagged gate, the pr_writer's summary and risks, a gate-evidence table for the pushed artifact, the plan by ID and version with both bases, and `Closes #<issue>`. The result carries `CIWait` (poll interval and timeout from the repository's `ci:` block), so the wait lives in history.
5. **The CI wait** (`featureRun.awaitCI`). The deadline is `workflow.Now` plus the timeout, set fresh on every entry. `ReadChecks` calls `github.App.ListCheckRuns` on the head commit, and `evaluateChecks` decides. Only runs whose App is the configured `check_app` count, and the newest run of each required name wins. A completed non-passing check fails at once. A completed read is appended to `pr_ci_results`, deduplicated by an observation hash of the runs seen. Between reads the workflow waits with `AwaitWithTimeout`, which wakes early for `/cancel`. A failure stops with `ci_failed`, and a deadline passed with `ci_timeout`.
6. **`MarkReady`** requires the latest CI result to be a pass on the head commit. It calls `github.App.MarkPullRequestReady`, which reads the PR, refuses one not opened by this App's bot, does nothing if it is not a draft, and otherwise sends the GraphQL `markPullRequestReadyForReview` mutation. `ready_at` is recorded, and the workflow moves to `delivered`.

A step with a `Stop` moves the feature request to `needs_attention` with `FeatureReason(stop, detail)`; an error goes there with reason `error`. `/retry` from such a stop runs `deliverPR` again from step 1 (ticket 8b's `resumeExecute` keeps the passed task), and each step resumes from the row.

### Storage

Migration `00009_pull_requests.sql` adds two tables:

- **`pull_requests`**: one row per delivered attempt, holding the identity, branches, original base and hash, and the pr_writer's output and commit message. The step columns are `landed_commit`, `base_sha`, `rebased_artifact_hash`, `head_commit`, `number`/`node_id`/`url` and `ready_at`. A trigger refuses any change to a filled one, even by the owner. The application role may UPDATE only those columns. Access goes through `store.SetPullRequestValue`, `SetPullRequestOpened` and `MarkPullRequestReady`, where the same value again is a no-op and a different value is `ErrAlreadyRecorded`.
- **`pr_ci_results`**: append-only, one row per completed observation. `LatestCIResult` is the PR's CI result. A re-run of CI followed by `/retry` adds a row rather than overwriting one.

### What each activity does after a crash

| Activity | Outside action | If the worker dies after it, before the record |
|---|---|---|
| `WritePullRequest` | model call | The reply is lost; the retry calls again and both calls are charged. Once the row exists, no further call is made. |
| `Land` | push to integration | The retry builds the same SHA, and a push of a commit the branch already holds succeeds. `landed_commit` is then recorded. |
| `Finalize` | none outside the worker | The base is recorded before the rebase and the hash before the gate. A retry reuses both, and `Gate` returns recorded results once every gate has a row. |
| `OpenPullRequest` | two pushes, PR create | The head commit is recorded before the pushes. Repeating a push to the same commit succeeds (`TestPushRepeatedForceIsIdempotent`), and `CreatePullRequest` returns the PR this App already opened from `pr-1`. |
| `ReadChecks` | none (a read) | The same runs give the same observation hash, so there is one row. |
| `MarkReady` | GraphQL mutation | The retry reads the PR, finds it not a draft, sends nothing, and records `ready_at`. |

`Activities.push` adds one more guard. If a push is refused for a stale lease, it pushes again with the target commit as the lease, which succeeds only if the branch already holds exactly that commit.

### Configuration

- `github.yaml` gains `bot_user_id`, validated non-zero and configured rather than looked up, so startup makes no network call.
- Each repository in `gates.yaml` needs a `ci:` block: `required_checks` (at least one), `check_app` (default `github-actions`), `poll_every` (default 1m, at least 10s) and `timeout` (default 60m, at most 24h).
- `tokenPermissions` now asks for `checks: read`.
- The worker registers `agent.PRWriter` in `roles()` and also registers `Finalize` on the attempt queue.

## Where to look

| File | What's in it |
|---|---|
| `internal/workflow/delivery.go` | Version gate, `deliverPR`, `awaitCI`, activity options |
| `internal/activity/delivery.go` | The six activities, `buildCommit`, `push`, `newBase`, `rebased`, `gateRebased` |
| `internal/activity/delivery_parts.go` | `commitMessage`, `renderPullRequestBody`, `evaluateChecks`, `newSandbox`, DB reads |
| `internal/activity/types.go` | `DeliveryInput`, `DeliveryStep`, `OpenResult`, `CIWait`, `ChecksResult`, the four new reasons |
| `internal/agent/prwriter.go` | `PRWriter` role, prompt, request, validated output |
| `internal/gate/rebase.go` | `Rebase`, `ErrRebaseConflict` |
| `internal/gate/policy.go` | `CIPolicy` and the `ci:` block |
| `internal/sandbox/commit.go` | `CommitDiff` |
| `internal/github/checks.go`, `pulls.go` | `ListCheckRuns`, `MarkPullRequestReady` |
| `internal/store/pullrequests.go`, `migrations/00009_pull_requests.sql` | The delivery record and CI results |

## What the tests prove

- **The whole path, run twice per step, is idempotent.** `TestDeliveryEndToEnd` (activity, real git, real Postgres) checks one pr_writer cost event, one rebase, one gate run, one PR and one CI row. Integration and `pr-1` both hold the head commit. The landed commit's parent is the pinned base and the head's is the new base. Both commits are by the bot, with the trailers and no forged one. The body has every section, and the draft is marked ready once.
- **An unmoved base reuses the original artifact.** `TestDeliveryBaseUnmoved` shows no rebase, no second gate, and a head equal to the landed commit.
- **A tampered artifact is never pushed.** `TestLandRefusesTamperedArtifact` rewrites the stored artifact on disk and checks that nothing is pushed. `TestDeliveryRefusesUnverifiedInput` refuses another hash or base before anything is written.
- **A conflict and a failed re-gate stop delivery.** `TestFinalizeStops` covers both, and for each one checks that `OpenPullRequest` fails and no PR exists. `gate`'s `TestRebase_DockerApplies` and `TestRebase_DockerConflict` run the real 3-way apply in Docker.
- **CI must pass, and only real CI counts.** `TestEvaluateChecks` is one case per rule, including another App spoofing a required check name. `TestMarkReadyNeedsPassingCI` covers a failed result. `TestCIPollsUntilTimeout` and `TestCIPassesAfterPolling` cover the timer loop.
- **The model cannot steer the commit or close an issue.** `TestPRWriterCannotForgeTrailers`, `TestPRWriterCannotCloseIssues`, `TestPRWriterRequestKeepsUntrustedOutOfSystem` and `TestParsePRWriterOutput` cover the pr_writer, and `TestWritePullRequestRepair` covers the one repair.
- **The pr_writer is a configured role.** `TestWritePullRequestUsesConfiguredRole` shows a worker without a pr_writer spec refuses with no model call, and a spec with `MaxSteps: 1` makes no repair.
- **`/retry` resumes without repeating work.** `TestDeliveryStopThenRetry` has one row per stop point and shows a `/retry` reaches delivered with one model call, one landing, one PR and one ready mark. `TestRebaseConflictRetryStopsAgain` shows a conflict stops again with no push or PR, and `TestCancelDuringCIWait` shows `/cancel` mid-wait reaches cancelled.
- **The records hold.** `TestPullRequestWriteOnceHoldsForOwner` and `TestAppRoleCannotRewritePullRequests` attempt the rewrites. `TestCommitDiffDeterministic` shows the same inputs give the same SHA.
- **Old and new runs replay.** `TestReplayMidAttempt` now also replays `testdata/feature-mid-ci.json`, a v9 run captured from Temporal while it waited on CI. The pre-9 fixtures still replay.

## Left open

- **Conflicts don't rerun the task.** FR-S19 says a finalization conflict reruns the task from the new base. Here it stops in `rebase_conflict`, and `/retry` meets the same conflict because the base is recorded write-once. `/cancel` is the way out until Milestone 4 (decision 0014).
- **Only check runs are read.** CI that reports through the commit statuses API is not seen, and lists over 1,000 runs are cut at the page limit.
- **Leftover branches from an earlier run block a new one.** If an earlier feature request on the same issue left `agent/<n>/integration` behind, `Land`'s lease refuses it and the run stops with reason `error`. Branch cleanup (FR-S20) is Milestone 4's.
- **The plan is named, not linked.** The body names the plan by ID and version and the issue it was approved on, because the plan comment's ID is not stored.
- **A crash can bill the pr_writer twice.** A worker that dies between the model's reply and the insert pays for a second call; the caps see both.
- **The Docker rebase test hasn't run on the real image.** It ran on the busybox test image only, not `deploy/images/java21`.
- **Manual owner steps:**
  - Grant the App `checks: read`.
  - Set `bot_user_id` from `GET /users/<slug>[bot]`.
  - Add a `ci:` block with ETS's real check names to `deploy/local/gates.yaml`. The worker refuses to start without it.
  - Confirm ETS's CI runs on pull requests or on pushes to `agent/**`.
  - Make sure `pr_writer` is in the repository's `repos.yaml` roles.

## Check your understanding

1. Why is the commit message stored on the `pull_requests` row instead of rebuilt in each step?
2. The worker dies after `OpenPullRequest`'s `CreatePullRequest` call succeeds. What happens on the retry, and which two mechanisms stop a second PR?
3. Why does the rebase run `git apply --3way` in a sandbox, not on the host, when the commits are built on the host?
4. What stops another GitHub App with `checks: write` from marking the PR ready?
5. After `ci_failed`, the owner re-runs CI on GitHub and sends `/retry`. Which activities do real work, and where does the new CI result go?
6. Why does a run started before this ticket still end in the stub's `needs_attention`?
