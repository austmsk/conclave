# Conclave M2 Design Doc — Revised Outline

Oct 30, 2026 · @Austin

This outline revises the original M2 plan with four changes: hidden tests are graded after the workflow instead of inside the M1 gate, datasets come from public PRs and authored tasks across five languages, results use multiple seeds with paired comparison, and the data model is slimmed down. Most design areas are now settled; the few still open are listed at the end.

## 1. Summary and principles

- M2 extends M1 and doesn't replace it: a thin evaluation control plane around the unchanged `TaskWorkflow`.
- Freeze what is tested, hide how it is judged, record enough provenance to explain every number, and report failures and exclusions honestly.
- Non-goals: PR delivery, multi-agent competition and judging, a second implementation loop.

## 2. Scope and requirement changes

- Replace "20+ owner-repo tasks" with internal tasks from recent public PRs plus authored tasks, across Python, Java, C++, Go and JS/TS. `docs/scope-and-requirements.md` line 38 still says "at least 20 tasks built from the owner's repositories".
- Keep the SWE-bench Verified subset, reported separately.
- Add requirements for multiple seeds per task and a paired comparison report.
- Reword FR-A2 ("applied only during gating") and exit-checklist item 5 (hidden tests as a gate overlay) to match the post-attempt grade step. Done with decision 0017, which also reworded FR-S7 and threat T2.
- Carry the M2 work the repo already assigns: FR-SEC27's `clean_room` vs. `reset_in_place` cost measurement, FR-SEC20's canary detector and the canary-hit halt alert (decision 0016), and revising the implementer's 20-minute wall clock from turn-latency data (decision 0016).

## 3. M1 integration map (T1 output)

Checked against the repo on Nov 16, 2026 (`main` after PR #29). A local run the same day passed `make check` and the CI form (`-race -count=1`, all 19 test packages).

| M1 seam | Status | M2 action |
| --- | --- | --- |
| `TaskWorkflow` without GitHub | Holds: `TestTaskWorkflowDirectly`, trees via `activity.TreeSource` | Reuse. The runner seeds repo, feature request, approved plan and task rows itself; benchmark rows need a synthetic issue number (`feature_requests.issue_number > 0`). |
| Worker and `activity.Deps` | GitHub-bound: startup requires a GitHub client, App key, alerts repository, bot identity and owner ID; `GitHubTrees` needs a GitHub repo ID | New: a benchmark worker with its own tree source and no-op GitHub and alerts, on its own task queue. |
| Config hashes in `plans.body` | Holds: SHA-256 of all six deploy files | Reuse. |
| Image pinned by digest | Partly: enforced, but `sandboxes.yaml` is keyed per repository | Extend: per-task image override, or one entry per SWE-bench instance. |
| Frozen, hashed diff | Holds: `artifact_hash` write-once; `gate.Load` fetches by repo ID and hash and re-verifies it | Reuse in Grade. |
| Gate staging and JUnit parsing | `stage`, `runGate`, `readReport` and `parseJUnit` are unexported; `parseJUnit` returns counts only | Extend: export staging; add a per-test JUnit parser so Grade can tell fail-to-pass from pass-to-pass. |
| Fixture storage | Blob namespaces must be repository UUIDs; no place for fixtures | New: a second blob directory the attempt worker never opens. |
| Grade sandbox ID | `attempts.sandbox_id` is write-once and belongs to the attempt | New: a record for the grade sandbox, so cleanup can find it. |
| Sweep clock | `attempts.started_at` is set when the row is created, before the attempt waits on the queue; the sweep releases sandboxes older than `-sweep-after` (2 hours) | Runner keeps in-flight workflows at or below the slot count, or the sweep keys on when the sandbox was recorded. Grade's timeout counts in `LongestSandboxHold`. |
| Effort per attempt | Missing: the adapter never sets it; Opus 5 defaults to high, Opus 5.5 to medium | New: `effort` on `llm.RoleRoute`, sent as `output_config.effort`, recorded in `contracts.Provenance` plus an `attempts` column (contract change, by hand). |
| Thinking blocks and refusals | Thinking blocks are discarded under a stale comment; `stopReason` treats a refusal as a provider error | Decide: thinking-block replay, and a refusal outcome. |
| Opus 5.5 route | Missing from `router.yaml` | Add a model entry. |
| Worker build version | Not recorded | Add to the run snapshot. |
| `test_writer` role | Contract exists; `roles()` and `TestRolesAreMilestoneOne` allow only implementer and pr\_writer | Keep: the production `roles()` is unchanged. `test_writer` goes in the evaluator worker's own role map (decision 0017). |
| Deploy files for benchmark repos | `sameRepositories` only checks that the loaded files agree, and every path is a flag; the `ci` block is required but never read by a benchmark | Separate deploy set for the benchmark worker, with a placeholder check name. |
| JUnit test reports | Already required by the gate (`/tmp/conclave-junit/`) | Reuse: matches the five-language plan. |
| Attempt throughput | One attempt slot per worker (decision 0012); the rate limiter is per worker process; the blob store is a local directory | Several attempt workers on one host or shared mount, with the rate limit divided across them. |
| Trial report query | `trial_report.sql` exists | Report generator starts from it. |

## 4. Architecture: gate vs. grade

1. Runner writes an intent row for each (task, seed).
2. Runner starts `TaskWorkflow` with a deterministic workflow ID. M1's gate runs visible repo tests only, as today.
3. The workflow ends with the frozen diff in the blob store, named by \`attempts.artifact\_hash\`.
4. A separate Grade activity runs a fresh container at the base SHA with a pinned image digest: apply diff, overlay hidden fixture, run tests, parse JUnit XML.
5. Runner writes the task result. Hidden-test output never goes back to the implementer.

- **Crash recovery:** intent row plus deterministic ID means a restarted runner reattaches instead of re-running. Grade is idempotent on (run\_task\_id, candidate\_digest), so a Temporal retry is always safe.

* **What Grade reuses:** `gate.Load` (artifact by hash, re-verified), `Trees.Prepare`, `Activities.newSandbox`, `gate.JUnitDir`, and the heartbeat and detached-write helpers.
* **What Grade adds:** exported staging and a per-test JUnit parser in `internal/gate`, a fixture overlay step, a per-task test command, and a record of its own sandbox ID.

## 5. Data model and provenance

- `plans.body` stays the authoritative record of what the agent saw; the manifest never duplicates or replaces it.
- Manifest: one YAML file per dataset in the repo, versioned by git commit plus content hash. `task_kind` values: `public_replay`, `authored`, `swebench_verified`.
- Three new tables only: `benchmark_run`, `run_task`, `fixture`.
- One canonical JSON run snapshot, stored with its hash: models, prompt hashes, config hashes, image digests, runner commit, rerun policy.
- Result identity binds the candidate diff digest, not just run ID and task ID.

## 6. Fixtures and hidden-test isolation

- Correctness source by task kind: public PRs use the PR's own tests as the reference; authored tasks use your original code as the reference; SWE-bench uses its official tests.
- `test_writer` drafts hidden tests blind to the reference fix and the candidate.
- Qualification: tests fail on base, pass on the reference, and existing tests still pass. Your manual sign-off is the final step before a task is eligible.
- Fixtures live in evaluator-only storage, never in the implementer's container, `plans.body`, logs, env vars or mounts.
- Isolation tests check each of those paths.

* The fixture store is a second blob directory that the attempt worker never opens.
* `SessionInput.PriorFailure` exists but nothing in production sets it today. An isolation test fails if grade output ever reaches `SessionInput`, so a later retry-with-feedback feature cannot wire the leak in.

## 7. Language support

- Grader parses JUnit XML only; each language is a sandbox profile plus a test command. Details in Conclave M2 — Multi-Language Dataset Plan.
- Dependencies are baked into pinned images, so grading never hits the network.
- Build order: Python, Go, JS/TS, Java, C++.

## 8. Datasets

| Source | Python | Java | C++ | Go | JS/TS |
| --- | --- | --- | --- | --- | --- |
| Recent public PRs | 4 | 4 | 4 | 4 | 4 |
| Authored tasks | 2 | 10 (M1 trials) | 1 | 2 (Conclave) | 1–2 |

- Public PR criteria: merged on or after Sep 1, 2026 (after Opus 5 and Opus 5.5 training data; provisional), linked to an issue, adds tests, small to medium size, repo not already in SWE-bench.
- Authored task methods: feature revert, bug injection, new work with acceptance tests. The 10 M1 trial issues on ETS (base 9839fab, in docs/trials/local/) are the first Java authored tasks; include all 10, the unresolved one too, to avoid survivor bias.
- Freeze each dataset version before scoring runs; a task never leaves a frozen version because a model failed it.

## 9. SWE-bench Verified subset

- Fixed list of 25 instances with benchmark version recorded.
- Official per-instance images, pinned by digest, as the sandbox image; official harness as the grader behind the Grade interface.
- Conformance check per instance: gold patch resolves, empty patch doesn't.
- Reported separately, with contamination and test-quality caveats stated.

## 10. Grading, flakiness and outcomes

- Each test carries an expected class: resolution (fail to pass), maintenance (pass to pass), setup check.
- Stability reruns happen at qualification; diagnostic reruns during scoring classify instability and never improve a score.
- Rerun counts (5 at qualification, 3 at scoring) are configurable and recorded in the run snapshot. A rerun-policy health check tracks when flips first appear and recommends raising or lowering the counts once enough flake events exist; changes apply only to new dataset or run versions, with owner approval.
- Outcome taxonomy kept from the original plan: resolved, unresolved, infra\_error, cancelled, budget\_exhausted, fixture\_invalid, excluded\_flaky, provenance\_invalid, runner\_interrupted.

## 11. Metrics and reporting

- 1 seed per task in development, 3 for official runs; mean pass@1 with a Wilson interval.
- Paired comparison report between runs on the same frozen tasks: per-task flips plus a bootstrap interval on the difference.
- Cost per resolved task = total eligible execution cost / resolved tasks. Fixture generation cost is tracked separately.
- Time per task (median, P95) and batch makespan reported separately.
- Model turn latency per call, to revise the implementer's 20-minute wall clock (decision 0016).
- Gating cost of `clean_room` vs. `reset_in_place` on the same artifacts, to set FR-SEC27's default from data.
- Breakdowns by dataset and language; a stated precision limit for the sample size.
- The report is a JSON file rebuilt only from stored records, starting from `trial_report.sql`; counts must reconcile with task rows.

## 12. Ticket plan

1. M1 close-out (tag, owed decisions) and contract inventory
2. Vertical slice: minimal runner, Grade activity, 3 hand-written Python tasks (dev-only results)
3. Persistence, run snapshot, intent rows, restart reconciliation; runner keeps in-flight workflows at or below the attempt slot count; benchmark worker without GitHub
4. `test_writer` fixture lifecycle, qualification, owner sign-off
5. Hidden-test isolation tests
6. Multi-language datasets in build order (Python → Go → JS/TS → Java → C++)
7. SWE-bench Verified subset
8. Seeds, comparison report, M2 exit checks

## 13. Risks

| Risk | Mitigation |
| --- | --- |
| Contamination inflates public-PR scores | Only PRs merged after tested models' cutoffs; authored tasks as an uncontaminated check |
| C++ and Java environments eat time | Build them last; prefer small builds and header-only libraries |
| Curation takes longer than coding | Shortlist repos early; \~30 to 60 minutes per authored task |
| Run cost: tasks × 3 seeds × 2 models, plus SWE-bench | Per-run budget cap; start with fewer seeds during development |
| Wall time: one attempt slot per worker, \~8 minutes per attempt; a full official run is \~44 hours serial | Several attempt workers; fewer seeds or one model until the pipeline is proven |
| The reaper releases live sandboxes of queued attempts: at one slot, roughly the 15th task onward passes the 2-hour sweep cutoff | Runner bounds in-flight workflows to the slot count (ticket 3) |
| N workers send N times the configured rate limit | Divide the per-process limit by the worker count |
| Opus 5 and 5.5 compared at different default effort (high vs. medium) | Set effort per route and record it per attempt |
| Hidden-test output reaches a later attempt (latent: `PriorFailure` is defined but unset in production) | Grade after the attempt, never in the M1 gate (decision 0017); isolation test on `SessionInput` |
| `test_writer` produces weak or wrong tests | Qualification against the reference plus owner sign-off |

## Open questions

These weren't settled in discussion and should be decided before drafting the full doc.

- [x] Decided: adopt the gate vs. grade split. Hidden tests run once after the workflow finishes, and nothing goes back to the implementer.
- [x] Decided: benchmark Opus 5 and Opus 5.5. Public PRs must be merged on or after Sep 1, 2026 (provisional until Opus 5.5's training-data cutoff is confirmed).
- [x] Decided: 1 seed in development, 3 for official runs. Per-run budget cap set after ticket 2 measures real cost per task.
- [x] Decided: public PRs use the PR's own tests as the hidden fixture; for benchmark fixtures, `test_writer` drafts hidden tests only for authored tasks.
- [x] Decided: start with 25 SWE-bench Verified instances.
- [x] Decided: 5 qualification runs and 3 scoring reruns, with a rerun-policy health check that recommends changes.
- [x] M1 assumptions checked (section 3).
- [x] Benchmark deploy entries: a separate deploy set on its own task queue, as decision 0012 anticipates.
- [ ] M1 close-out: tag `m1-complete`; write decisions 0002, 0003 and 0005; accept or revise 0008 through 0016
- [x] Decision 0017, accepted 2026-11-24: hidden tests run in a separate Grade step after the task, on an evaluator-only worker. `test_writer` is benchmark-only in Milestone 2, and a gate-failed task's Grade result is kept for diagnosis only.
- [ ] Effort value per route for Opus 5 and Opus 5.5 (the same explicit value on both)
- [ ] Thinking blocks: keep discarding them, or replay them (changes `contracts.Message`)
- [ ] A `refusal` outcome in the taxonomy, so a refused call is not counted as a provider error
- [ ] How many attempt workers for official runs, given the per-process rate limit and the shared blob directory
- [ ] Verify recent PR activity and shortlist repos per language
