# Glossary

Terms as this project uses them. Where a term maps to code, the code location is given.

## Work and lifecycle

**Feature request.** One labeled issue and everything done for it. It has six statuses (`contracts.FeatureRequestStatus`), and its Temporal workflow ID is derived from the issue number.

**Plan.** A versioned list of tasks for a feature request. At most one version is `approved` at a time, which a partial unique index enforces. Approval pins the base SHA. In Milestone 1 a plan always has exactly one task.

**Task.** One unit of work from an approved plan (`contracts.TaskSpec`), with acceptance criteria a test can check.

**Attempt.** One try at a task, in one sandbox, by one implementer. It ends `passed`, `failed` (the agent's fault), or `errored` (the platform's fault). Keeping `failed` and `errored` apart keeps resolve rates honest.

**`needs_attention`.** Where every problem goes: a failed attempt, a non-retryable error, a budget cap. It always carries a `reason`. There is no automatic `failed` status. The owner decides with `/retry`, `/revise` or `/cancel`.

**Base SHA.** The commit a plan was approved against. Every task runs against it, so work doesn't shift if the default branch moves mid-run.

**Delivered.** The workflow's last status: the PR is open. Merging happens outside the workflow.

## Database

**Compare-and-set (CAS).** `UPDATE … WHERE id = $id AND state = $expected AND revision = $rev`. If no row changed, someone else moved the record first. `store.Transition` does this for every state change (`RL-13`).

**Revision.** A counter on each record, bumped by the database trigger on every state change. Matching on state alone isn't enough, because lifecycles are cyclic (`running → needs_attention → running`). A stale retry could otherwise land on a record that has since gone round the cycle.

**`ErrAlreadyApplied` vs `WrongStateError`.** These are the two outcomes when a CAS updates nothing. "My earlier try already did this" is success. "Someone else changed it" means the caller lost a race. Telling them apart is the reason the pattern exists.

**Transition table.** The maps in `internal/contracts/state.go` that list each state's legal next states. They are the single source of truth: the store checks them before running any SQL.

**Audit row / `state_transitions`.** Written by the trigger `record_state_transition`, not by Go code, so no state change can skip it. `actor` is whatever the application reported. `db_user` is the login Postgres authenticated, which the application can't forge.

**Append-only.** `cost_events` and `state_transitions`: the application role (`conclave_app`) can insert and read them, but never update or delete.

**Idempotency key.** A unique key chosen before a side effect, so a retry that repeats it is ignored (`ON CONFLICT DO NOTHING`). This is how a cost is never recorded twice.

## Sandbox

**Sandbox.** A hardened Docker container where the agent's commands run (`contracts.Sandbox`, `internal/sandbox/docker`). Untrusted code runs only here.

**Prepared tree.** A clone with remotes, credential helpers and reflogs stripped (`sandbox.PrepareTree`), so nothing from the trusted workspace follows the code into the container.

**Two-phase network.** Install dependencies with network access, then disconnect before the agent gets control (`CreateWithInstall`). `Create` alone never gives a network.

**Reaper.** `Provider.Reap`: removes labeled containers and networks older than a given age, so a worker that died mid-attempt leaves nothing running.

**Disconnected context.** `workflow.NewDisconnectedContext`, used for cleanup so it still runs after the workflow is cancelled.

## Models

**Role.** A named agent job: `implementer`, `planner`, `judge`, `test_writer`, `pr_writer`, `discussion`, `brief`, `conflict_resolver` (`contracts.Role`). Roles are data, not code: one agent loop runs them all, with a different prompt, tools and limits for each.

**Router.** `internal/llm`. Every model call goes through it, so tier enforcement happens in exactly one place (`RL-17`).

**Sensitivity tier.** Which providers may see a repository's code: `public`, `private`, `sensitive` (an owner-approved list), or `local_only`. The owner sets it in `deploy/repos.yaml`. It is checked on every call, including fallbacks. No tier means no run.

**Scope / Scoped router.** `llm.Scope` binds a router to one repository and, normally, one attempt. It carries the token budget and the idempotency key prefix. Callers get a `*Scoped` from `Router.Scope` and call `Complete` on that.

**Fallback chain.** A role's main model plus fallbacks. A fallback is tried only when the previous model was *unavailable*, and only if the tier allows it. A disallowed fallback ends the request; it is never skipped.

**Permanent error.** An error a retry can't fix (`llm.IsPermanent`), such as a tier denial, a bad config, or an exhausted budget. Ticket 8 turns these into non-retryable Temporal errors.

**Prompt version.** A label on every request naming the version of the role's instructions, so results stay comparable when prompts change (decision record 0006).

**Fake replay provider.** `internal/llm/faketest`: serves recorded responses in order and fails if the conversation drifts from the recording. No test calls a real model.

## Process

**Ticket.** One unit of the build plan (`docs/tickets/milestone-1.md`), done in one Claude Code session on a `ticket/<n>` branch.

**`FR-*` / `NFR-*`.** Functional and non-functional requirement IDs (`docs/scope-and-requirements.md`). `FR-SEC*` are the security requirements.

**`RL-*`.** Review-list items (`docs/review-list.md`): the topics where plausible-looking code is most often wrong.

**Decision record.** A short note in `docs/decisions/` explaining one non-obvious choice, written by the owner after merge (0001).

**Canary.** A fake secret in the `canary_*` format, planted so a test can prove it never leaks.
