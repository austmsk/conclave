# 0014 — Pull request delivery: delivered after CI, polled checks, a sandboxed rebase, and deterministic commits

**Date:** 2026-10-31
**Status:** proposed

## Context

Ticket 8 ended every passed task at a delivery stub that verified the artifact and stopped in `needs_attention`. Ticket 9 delivers it: one squashed commit on the integration branch, a rebase onto the default branch with the gates run again, a draft pull request, and ready for review once the repository's CI passes (FR-S10, FR-S16, FR-S17, FR-S19, FR-A5, FR-A7, FR-SEC27). These choices had to be made:

- **When the feature request counts as `delivered`.** When the pull request opens, or when its CI passes.
- **How CI is observed.** The webhook receiver knows only `ping`, `issues` and `issue_comment`.
- **Where the rebase runs, and what a conflict does.** FR-S19 says a conflict reruns the task from the new base, but the plan's base is pinned write-once.
- **How a retry avoids a second push, pull request or model call** (RL-2), including the owner's `/retry` from ticket 8b, which re-enters delivery from its start.
- **Where the commit author, the CI settings and the delivery state live.**

The owner decided, on 2026-10-30 and 2026-10-31:

- `delivered` comes after CI passes.
- CI is read by polling the checks API.
- A rebase conflict goes to `needs_attention` with reason `rebase_conflict`.
- CI results go in an append-only table.
- `/retry` after `rebase_conflict` is allowed to fail again.
- The bot's user ID is configured, not looked up.

## Decision

- **Delivered after CI passes.** The sequence is six activities: `WritePullRequest`, `Land`, `Finalize`, `OpenPullRequest`, `ReadChecks` on a timer, and `MarkReady`. The pull request opens as a draft. When every required check passes, it is marked ready and the feature request moves `executing → delivered`. A required check that completes without passing stops in `needs_attention` with `ci_failed`. Checks still running when the timeout passes stop with `ci_timeout`. Both leave the draft open.
- **CI is polled, not pushed.** The workflow sleeps `poll_every` between `ReadChecks` calls, up to `timeout`. Each call reads `GET /repos/{owner}/{repo}/commits/{sha}/check-runs` for the pull request's head commit.
  - The installation token asks for `checks: read`, and the App's settings must grant it (FR-SEC15). Checks are never written.
  - Only check runs from the configured `check_app` count, by default `github-actions`. Another App with `checks: write` cannot pass CI by naming a run after a required check.
  - CI that reports through the commit statuses API is not seen. ETS runs on Actions, so this is fine for Milestone 1.
  - A required check that completes without passing fails CI at once, without waiting for the rest.
- **A per-repository `ci:` block in `gates.yaml`.** It sets `required_checks` (at least one, required), `check_app` (default `github-actions`), `poll_every` (default 1m) and `timeout` (default 60m). `OpenPullRequest` returns the wait settings, so they are recorded in history and a configuration change cannot alter a run in flight (RL-16).
- **The rebase runs in a sandbox.** `Finalize` records the default branch's head as the new base, write-once. In a fresh sandbox over a clone at that base, it applies the original diff with `git apply --3way --binary`. It then freezes the result there with `gate.Freeze`, so the diff bytes come from the same git the gate's round trip uses. The rebased artifact is gated `clean_room` by `gate.Gate`, unchanged, and its hash is recorded write-once. If the base did not move, the original artifact and its gate results stand.
- **A conflict stops; it does not rerun.** A non-zero `git apply --3way` stops in `needs_attention` with `rebase_conflict`. This deviates from FR-S19: rerunning the task needs a new pinned base, and the plan's base is write-once. The rerun is deferred to Milestone 4. A `/retry` meets the same conflict and stops again, with no push and no pull request, so `/cancel` is the way out until then. A rebased artifact that fails a gate stops with `rebase_gate_failed`.
- **Deterministic commits, built on the host.** The commit is built in a fresh clone with a temporary index: `git read-tree <base>`, `git apply --cached --binary`, `git write-tree`, `git commit-tree`. The work tree is never written, and git runs with isolated configuration.
  - Author and committer are the App's bot: `<slug>[bot]`, `<bot_user_id>+<slug>[bot]@users.noreply.github.com` (FR-S17).
  - Both dates are fixed to `attempts.finished_at`.
  - With a fixed message, tree, parent and dates, a retry builds the same commit SHA, and pushing a commit the branch already holds is a no-op.
- **The commit message is built once and stored.** `WritePullRequest` builds it from the pr_writer's output and stores it, write-ahead, with the title, summary and risks. It is part of every commit SHA, so it is never rebuilt.
  - The subject is a Conventional Commits type from a fixed list and a description of at most 72 characters, with no newline.
  - Trailer-shaped lines are stripped from the model's body, so the model cannot forge a trailer.
  - No field the model writes may close an issue. After a GitHub closing keyword (close, fix, resolve and their forms), the reference is rewritten as plain words: `Fixes #12` becomes `Fixes issue 12`, and `owner/repo#N`, `GH-N` and issue or pull URLs become `owner/repo issue N` or `issue N`. This covers the subject, body, title, summary and risks. The title counts because a squash merge uses it as the commit subject. The only closing reference left is the platform's `Closes #<issue>`.
  - The trailers are `Task: <task id>`, `Plan: <plan id> v<version>`, `Model: <attempts.model>` and `Generated-by: conclave`. The last is also the commit's AI label (FR-C3).
- **The branch model** (FR-S16).
  - `Land` pushes the commit on the pinned base to `agent/<issue>/integration`, with lease `""` (the branch must not exist).
  - After the rebase, `OpenPullRequest` builds the commit on the new base. It force-pushes integration with lease set to the landed commit, and pushes `agent/<issue>/pr-1` with lease `""`. Both point at the rebased commit.
  - It then opens the draft pull request from `pr-1`. The body is rendered by the platform, in this order: the AI label (FR-C3); a security-relevant changes section listing every flagged gate, execution-surface changes included (FR-SEC19, FR-SEC25); the pr_writer's summary and risks; the gate evidence for the pushed artifact; the plan, by ID and version, naming the issue it was approved on and both bases; and `Closes #<issue>`. The plan is named rather than linked, because the plan comment's ID is not stored.
- **The `pull_requests` table** (migration 00009). There is one row per delivered attempt. It holds the branches, the original base and artifact, the pr_writer's output and the commit message. Each step then fills its own columns once: the landed commit, the base SHA (FR-S18a), the rebased artifact hash, the head commit, the pull request's number, node ID and URL, and `ready_at`. A trigger refuses any change to a column that holds a value, and the application role may update only those columns.
- **The append-only `pr_ci_results` table.** There is one row per completed CI observation. A retried read of the same check runs is absorbed by an observation hash over their IDs and conclusions. A re-run of CI has new run IDs, so it records a new row. The latest row is the pull request's CI result.
- **`bot_user_id` is configured** in `github.yaml`, where a zero placeholder fails validation, like `owner_user_id`. It is not looked up at startup, so startup makes no network call. The owner finds it with `GET /users/<slug>[bot]`.
- **The pr_writer runs under a feature-request scope.** It is outside any attempt, so it has no attempt budget, and the feature request and monthly caps bound it (FR-SEC9). A cap refusal stops in `needs_attention` with `budget_cap`. Its spec comes from `Deps.Roles`, as every role's does, so a worker without a pr_writer spec refuses before any model call. The spec's limits bound the calls: at most `MaxSteps` (2: the call and one repair), a repair only while the calls so far used fewer than `MaxTokens`, and all within `MaxWallClock`. Its output is validated and stored, so a retry never calls the model again.
- **A version gate.** `FeatureWorkflow` reads `GetVersion("delivery-9", DefaultVersion, 1)` once, at the start, as 8b does. A run started before this ticket keeps the stub and stops in `needs_attention` as before. A new run delivers.
- **`/retry` re-enters at `WritePullRequest`.** After a delivery stop, `/retry` runs the whole sequence again with the kept task result, and no new attempt. Every step resumes from the row: a finished step returns what it recorded and acts on nothing.
  - After `ci_failed`, the owner re-runs CI on GitHub and sends `/retry`. Delivery goes straight to polling, with a fresh timeout.
  - After `ci_timeout`, `/retry` polls again with a fresh timeout.
  - After `rebase_conflict`, the conflict recurs (see above).
  - After `error` or `rebase_gate_failed`, the steps before the stop return their recorded results, and the failed step runs again.

## Alternatives considered

- **A `check_suite` or `workflow_run` webhook instead of polling.** This was rejected for Milestone 1. It needs a new event in the receiver, a new signal, and a fallback for a missed delivery, which is a poll anyway. Polling once a minute for an hour costs at most 60 small activities per run.
- **`delivered` when the pull request opens.** Rejected: FR-A7 says a PR is ready only when real CI passes. A run that ended `delivered` with red CI would need a second status to say so.
- **Rebasing on the host.** It is cheaper, with no sandbox. Rejected because the gate checks that the diff reproduces byte for byte in the sandbox, and host and image git can differ in output, binary patches above all, since they are zlib-compressed. A host-made diff could then fail the gate for a reason unrelated to the work.
- **Rerunning the task on conflict, as FR-S19 says.** Deferred. It needs a new base pinned for the plan, which is write-once by design, and a task rerun from delivery. Milestone 4's planner and integration work is where the base becomes per-task.
- **A CI result column on the pull request row.** Rejected: a write-once conclusion cannot record a re-run after `/retry`, and an overwritable one loses history.

## Consequences

- **Deferred:** restacking (FR-S18a, though the base SHA is recorded now, so it needs no migration), branch cleanup on merge and after 30 days (FR-S20), stacked pull requests (FR-S18), and the API commit path for repositories that require signed commits (FR-S17).
- **Manual owner steps:** grant `checks: read` in the App's settings, and set `bot_user_id` in the real `github.yaml`. Add a `ci:` block for each repository to the real `gates.yaml`. Confirm the repository's CI runs on `pull_request`, or on pushes to `agent/**`.
- **History grows with CI time:** one `ReadChecks` per poll. At the defaults that is at most 60 per delivery, well inside Temporal's limits without continue-as-new.
- **A dead worker after the pr_writer replied, before the row was written,** loses the reply. The retry calls the model again, and both calls are charged and capped.
- **Only check runs are read.** A repository whose CI reports commit statuses, or runs under another App, needs `check_app` set, or its own support later.
