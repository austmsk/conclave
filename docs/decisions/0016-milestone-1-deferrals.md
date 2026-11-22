# 0016 — What Milestone 1 defers, and the known gaps it leaves

**Date:** 2026-10-31
**Status:** accepted

## Context

Milestone 1 claims a set of requirements (`m1-exit-checklist.md`), and several others that the specification places early were moved later while the milestone was built. Item 10 of the checklist asks that each be recorded as a decision, "so a later session does not treat them as bugs". Some were already recorded in a ticket's own record; this one gathers them in one place and adds those ticket 10a made. Separately, the ticket 9 review left findings the owner chose not to fix. Those are gaps, not choices about scope, and are listed apart so they are not mistaken for either.

## Decision

### Deferred, with where each lands

| Requirement | What is deferred | Why | Lands in |
|---|---|---|---|
| FR-S18a | Restacking PRs above a merged one | Milestones 2 and 3 never produce a stack. The base SHA is already recorded write-once at branch creation, so restacking needs no migration (0014). | Milestone 4 |
| FR-S20 | Branch cleanup on merge, and the 30-day `agent/` branch reaper | Nothing merges automatically in Milestone 1, and the owner deletes branches by hand. A leftover `agent/<n>/integration` stops a new run on the same issue with reason `error`. | Milestone 4 |
| FR-S19 (rerun) | Rerunning a task after a finalization conflict | Rerunning needs a new base pinned for the plan, which is write-once by design. A conflict stops in `rebase_conflict` instead (0014). | Milestone 4 |
| FR-S18, FR-S17 (signed) | Stacked PRs, and the API commit path for repositories that require signed commits | One task, one PR, in Milestone 1. ETS does not require signed commits (0014). | Milestone 4 |
| FR-I5 (tracing) | OpenTelemetry export | Runs are traced through the Temporal UI and the database (`TestTraceableFromWorkflowID`). Jaeger runs in Compose but nothing exports to it. | After Milestone 1 |
| FR-SEC17 | Pre-run secret and personal-data scans before content reaches a model | The gitleaks gate scans the agent's output, not the input. ETS is the owner's own private repository. | After Milestone 1 |
| FR-SEC20 | Canary tokens and their detector | The detector and prompt-injection fixtures belong with the evaluation harness. Tests use the `canary_*` format already. | Milestone 2 |
| FR-SEC22 | Excluded-path substitution | No Milestone 1 repository has excluded paths. | After Milestone 1 |
| FR-SEC30 (build) | CI build of sandbox images, the weekly rebuild and scan, and GHCR publication | The java21 image is built locally and pinned by digest. Workers already pull by digest only, enforced at startup (0011). Until the build exists, the owner keeps a `docker save` archive of each pinned image (0011, amendment of 2026-11-21). | Milestone 2, before its first new image (0011, amendment) |
| FR-SEC31 (auto-halt) | Halting on "repeated authorization failure from an unknown actor" | No threshold is specified. The webhook server, which sees the failures, has no database access to record a halt (0015). Refused commands and labels are logged by the receiver. | After Milestone 1 |
| FR-SEC31 (digest) | Batching `digest` alerts weekly | Milestone 1's only digest alert is `conclave alert-test`, delivered at once. | After Milestone 1 |
| FR-SEC31 (canary) | The canary-hit `halt` alert | It needs FR-SEC20's detector. | Milestone 2 |
| FR-SEC31 (channel) | A second alert channel beyond GitHub | Already an accepted residual risk. The local log holds every alert when GitHub is down or suspect. | Not planned |

### Known gaps from the ticket 9 review

These were found in review and left unfixed by the owner's choice on 2026-10-31. Each is a defect or a missing test, not a deferral. A later ticket that touches the code should fix it rather than build on it.

- **Skipped and neutral checks count as passing CI.** `evaluateChecks` treats `skipped` and `neutral` conclusions on a required check as a pass (FR-A7).
- **An empty risks section is allowed.** The pr_writer may return no risks, and the PR body then has an empty section (FR-S10).
- **`WritePullRequest` has no `MaximumAttempts`.** It retries until its schedule-to-close bound, and each retry before the row exists may call the model again. The caps still bound the spend.
- **`DeliveryInput.TaskID` is unused.**
- **The GraphQL URL is built from the REST base URL**, which is wrong on GitHub Enterprise Server. Only github.com is supported in Milestone 1.
- **`minPollEvery` is defined twice**, in the gate policy and in the workflow.
- **No test covers the pr_writer's budget-cap stop** in delivery. Ticket 10a's `RaiseAlert` call covers both cap paths through `needsAttention`, but only the task path has a workflow test.

### Found in the ticket 10b trials

The live trials (2026-11-16) turned up two defects, both fixed on the 10b branch, and five behaviours that are left as they are. The defects are listed so their tests are found; the rest are choices or gaps.

- **Fixed: the implementer was never told its constraints.** `SessionInput.Limits` reached the agent package but not the prompt, so an attempt could not know the 400-line cap it failed against. The task message now states the cap, protected and execution-surface paths and the dependency policy, and the prompt version is `implementer-v2` (agent `TestTaskMessageStatesConstraints`). Attempts before the fix are recorded as `implementer-v1`.
- **Fixed: nested bullets split an acceptance criterion.** A criterion written as a lead-in with sub-bullets became one criterion per bullet (activity `TestParseIssueNestedItems`).
- **Left: the implementer's 20-minute wall clock.** One trial stopped twice on `wall_clock`, with every tool call under two seconds and model turns of up to 23k output tokens taking one to three minutes each. `MaxWallClock` is one of `docs/agent-roles.md`'s starting values, to be revised with Milestone 2 data, so it was not raised during the trials. Milestone 2's benchmark should measure turn latency before choosing a new value.
- **Gap: CRLF files cost the agent turns.** In both of that trial's attempts the agent inspected and then converted the line endings of the CRLF files it had to edit, which would also have rewritten every line against the diff cap. Nothing tells the agent a file's line endings, and the file tools neither preserve nor report them. A later ticket could state the line endings in `read_file`'s result or the repository profile.
- **Gap: the implementer is told one of its three size caps.** `checkDiffSize` (`internal/gate/size.go`) fails a diff over `max_changed_lines`, `max_changed_files` or `max_diff_bytes`, but `contracts.Constraints` carries only `MaxChangedLines`, so the Constraints section states the line cap alone. The section's lead-in ("The platform's checks enforce these after the session ends") and the system prompt ("the only statement of the limits") both read as complete, so an attempt can still fail, as trial 6's did, on a file-count or diff-size cap it was never told. Stating them needs `MaxChangedFiles` and `MaxDiffBytes` on `contracts.Constraints`, a contract change that is reviewed by hand and was outside 10b. The fix adds both fields, fills them in `Policy.Constraints` (`internal/gate/policy.go`), and has `writeConstraints` (`internal/agent/prompt.go`) state each with how it is counted (files the diff touches, binary ones included; bytes of the raw diff), under a new prompt version. A plan approved before then has neither field, so each is stated only when positive, as the line cap already is.
- **Left: cancellation waits for a heartbeat.** `/cancel` and a halt reach a running attempt only on its next heartbeat (every 30 seconds), so an attempt stops up to about 40 seconds after the request. This is how Temporal delivers activity cancellation, and the sandbox is released in every case.
- **Cosmetic: alert-log times are not all UTC.** Alert lines whose time comes from Postgres are written with the worker's local offset, others in UTC. Each is the same instant and parses the same.

## Consequences

- A session that finds one of these behaviours should check this record before treating it as a bug.
- The Milestone 2 tickets that pick up FR-SEC20 and the canary alert, and the Milestone 4 tickets for FR-S18a, FR-S19 and FR-S20, start from the seams listed in `m1-exit-checklist.md`. Milestone 2 also inherits the wall-clock and CRLF findings from the ticket 10b trials.
- Decisions 0002 and 0003 (ticket 2) are still owed by the owner. NFR-M9 is not met for Milestone 1 until they are written.

## Amendment, 2026-11-21: corrections, cost data, and what was found after the milestone closed

Reviewed after Milestone 1 closed and accepted.

**Corrected.** The last consequence above no longer holds: decisions 0002 and 0003 were written and accepted on 2026-11-21, so NFR-M9 is met for Milestone 1. The FR-SEC30 (build) row lands at the start of Milestone 2, per 0011's amendment, which also adds that the image pipeline never publishes an image for a `local_only` repository.

**Cost data for the wall-clock finding.** A read-only query over the trials' `cost_events` and `attempts` (10 trials, 154 model calls, $51.41) puts numbers on it:
- The two wall-clock stops on issue #17 were the most expensive outcome: $12.97 with nothing delivered.
- They were output-heavy: about 95,000 to 106,000 output tokens per attempt, against 15,000 to 31,000 for a passing one. Output is billed at five times input, so long model turns are the costliest failure, not only the slowest.
- About half the trials' spend, roughly $25.60, went to attempts that did not deliver: the two wall-clock stops, a gate failure that cost $8.88 after 37 steps, and the two cancelled trials.
- On passing attempts, 65% of implementer cost was input: each step resends 13,000 to 19,000 tokens, uncached (decision 0008, item 5).
- Among the seven passing attempts, cost tracked steps (correlation 0.92) and diff size (0.84). Seven is too few for more than direction.

Milestone 2 should weigh these alongside turn latency when it revises `MaxWallClock`.

**Found after the milestone closed.** Each item is recorded in the decision named, so a later session should look there before treating it as a bug:

| Finding | Recorded in |
|---|---|
| Temporal has no authentication: a client that can reach it can signal a workflow past the owner check. The dev stack is bound to `127.0.0.1`; a second machine needs Tailscale ACLs or mTLS first | 0002, "Left for later"; `docs/running-locally.md` |
| `db_user` is recorded on `state_transitions`, `halt_events` and `commands`, not yet on `cost_events`, `tool_calls` or `pr_ci_results`; a change to `repos.tier` writes no audit row | 0002, "Left for later" |
| The adapter sends no effort, drops thinking blocks, fails a refusal as an unsupported reply, does not stream, and requests no prompt caching; a future Ollama adapter must validate its endpoint as local | 0008, amendment |
| The java21 image was held only by the owner's Docker daemon; it now has a private GHCR copy and a local archive until CI builds images | 0011, amendment |
| The sweep's cutoff covers how long an attempt runs, not how long it waits on its queue, so a benchmark backlog could have its sandbox released | 0012, amendment |
| One monthly cap is shared by live work and the benchmark; cap-stopped benchmark tasks stay in `needs_attention` unless the runner moves them | 0009, amendment |
| The halt covers the benchmark only within the same Temporal namespace | 0015, amendment |
| `/retry` reads as starting over; `/continue` becomes the owner's command, with `/retry` as an alias | `docs/tickets/m2-tickets/continue-command.md` |
