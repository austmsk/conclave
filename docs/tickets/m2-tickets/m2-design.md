# Milestone 2 — Evaluation Harness: Design

**Status:** draft, 2026-11-26. The owner confirmed its twelve main proposals the same day (section 15); the rest of the document has not had a full review. It expands the [revised outline](Conclave%20M2%20Design%20Doc%20—%20Revised%20Outline-2.md), which stays as the record of what was decided and when. Where the two disagree, the outline's decisions win and this document is wrong.

**Done when:** a frozen benchmark of internal tasks and a SWE-bench Verified subset has been run on two models with three seeds each, and a generated report gives resolve rate, cost per resolved task and time per task, with every number traceable to stored records (FR-S7, FR-A2, FR-A4, NFR-P6).

**Markers used below.** *Decided* means the owner settled it, in the outline or in [decision 0017](../../decisions/0017-test-writer-and-hidden-test-grading.md). *Confirmed* means this draft proposed it and the owner agreed on 2026-11-26. *Proposed* means this draft chose it and the owner has not yet agreed. Section 15 lists every proposal and every open item in one place.

## 1. Summary

Milestone 1 turns a labelled issue into a pull request. Milestone 2 measures how well it does that. It adds a thin evaluation layer around the Milestone 1 task pipeline and changes that pipeline as little as possible, so the numbers describe what production runs.

Four principles, all *decided*:

- **Freeze what is tested.** Tasks, base commits, fixtures, images and configuration are fixed and hashed before a scoring run.
- **Hide how it is judged.** Hidden tests never reach an implementer, and their results never cause another attempt.
- **Record enough to explain every number.** Each result names the models, effort, prompts, configuration, images and the exact diff it graded.
- **Report failures honestly.** Refusals, infrastructure errors, flaky fixtures and exclusions are counted and shown, not dropped.

**Non-goals:** pull request delivery, competition between agents, judging, and a second implementation loop. Those belong to Milestones 3 and 4.

## 2. Scope and requirement changes

**In scope**

- A benchmark runner, an evaluator worker and a Grade step.
- The `test_writer` role, used to draft hidden tests for authored benchmark tasks.
- Internal datasets in three languages (Python, JS/TS and Java), from recent public pull requests and authored tasks. Go and C++ are deferred (owner, 2026-11-26).
- A SWE-bench Verified subset of 25 instances, reported separately.
- Three seeds per task on official runs, and a paired comparison between runs.
- Work earlier decisions assigned to this milestone:
  - the `clean_room` vs. `reset_in_place` gating cost measurement (FR-SEC27);
  - the canary detector and the canary-hit halt alert (FR-SEC20, decision 0016);
  - model turn latency, to revise the implementer's 20-minute wall clock (decision 0016);
  - the sandbox image build in CI, before the first new image (decision 0011, amendment).

**Requirement changes already made** (decision 0017): FR-A2, FR-S7, threat T2, the `test_writer` row in `agent-roles.md`, and exit-checklist item 5.

**Requirement changes still to make** (wording *confirmed*; not yet applied to those files):

| Where | Today | Confirmed wording |
|---|---|---|
| `scope-and-requirements.md`, Milestone 2 success criteria | "A replay benchmark of at least 20 tasks built from the owner's repositories, plus a SWE-bench Verified subset" | "A frozen benchmark of at least 30 internal tasks across Python, JS/TS and Java, drawn from recent public pull requests and authored tasks, plus a SWE-bench Verified subset reported separately" |
| Same list | (none) | "Official results use at least three seeds per task, and two runs on the same frozen tasks can be compared task by task" |
| `m1-exit-checklist.md`, "What Milestone 2 must find in place" lead-in | Repeats the "20 tasks from the owner's repositories" wording | A dated note pointing here |

## 3. What Milestone 1 provides

The outline's section 3 checked every seam against the code on 2026-11-16. This is the short form; the outline has the evidence for each row.

| Seam | State | Milestone 2 action |
|---|---|---|
| `TaskWorkflow` runs without GitHub | Holds | Reuse unchanged |
| The plan body is the frozen agent input, with config hashes | Holds | Reuse unchanged |
| Frozen, hashed diff (`attempts.artifact_hash`, `gate.Load`) | Holds | Reuse in Grade |
| JUnit reports from the gate | Holds, but the parser returns counts only | Add a per-test parser |
| Gate staging | Unexported | Export it for Grade |
| Worker startup | Requires GitHub credentials | Add a benchmark worker without them |
| `sandboxes.yaml` | Keyed per repository | Allow a per-task image |
| Blob store | One directory, namespaced by repository | Add a second directory for fixtures |
| Sweep cutoff | Counts queue wait as sandbox age | Bound in-flight work in the runner |
| Effort, thinking replay, refusals | Missing or wrong | Three contract changes, section 12 |
| `trial_report.sql` | Exists | The report generator starts from it |

## 4. Architecture

### 4.1 Processes

Three kinds of process take part in a benchmark run. The split between the second and third is the trust boundary decision 0017 sets.

| Process | Task queue | Holds | Runs |
|---|---|---|---|
| Runner (`conclave bench`, a CLI) | none | Database access, a Temporal client | Starts and reattaches to workflows, bounds in-flight work, writes the report |
| Benchmark attempt worker | `bench`, `bench-attempt` | The attempt blob directory, model credentials, a local tree source | `TaskWorkflow` and its activities, exactly as in Milestone 1 |
| Evaluator worker | `bench-eval` | The **fixture store** (fixtures and reference fixes), the attempt blob directory (read-only use), model credentials for `test_writer` | Grade, `test_writer` sessions, result recording |

- The benchmark attempt worker is the Milestone 1 worker built with a different `activity.Deps`: a tree source that clones from a local mirror, and no-op GitHub and alert clients. It is never given a fixture-store path, and it refuses to start if one is set.
- Both workers read a separate deploy set (`deploy/bench/`), so benchmark repositories, images and routes never mix with production ones. *Decided.*
- The production worker is not touched. Its `roles()` stays implementer and pr_writer.

### 4.2 Flow of one scored task

*Confirmed shape:* one small workflow per (task, seed), `BenchTaskWorkflow`, on the `bench-eval` queue. The outline says the runner starts `TaskWorkflow` and then Grade runs; wrapping both in one workflow gives the same order and lets Temporal, not the runner, remember where each task is.

1. The runner writes a `run_task` row in state `pending`. This is the intent row.
2. The runner starts `BenchTaskWorkflow` with a deterministic ID, `bench/<run id>/<task id>/<seed>`. Starting it twice is a no-op, so a restarted runner reattaches.
3. The workflow seeds the Milestone 1 rows through an activity: repository, feature request (with a synthetic issue number), approved plan and task. Seeding is keyed on the `run_task` ID, so a retry finds the rows it already wrote.
4. The workflow starts `TaskWorkflow` as a child on the `bench` queue. The implementer runs, the Milestone 1 gate runs on the repository's visible tests, and the final diff is frozen under `attempts.artifact_hash`.
5. When the child completes, the workflow runs the **Grade** activity on `bench-eval`.
6. A last activity writes the outcome to `run_task`. Sandbox cleanup for Grade runs in a deferred, disconnected context.

Grade output never flows back toward step 4. `BenchTaskWorkflow` passes IDs, hashes and counts only (RL-14).

### 4.3 The Grade activity

*Decided* in 0017; the steps here add detail.

1. Read the candidate. On a scoring run it is the final ordinal's `artifact_hash`, in the attempt blob directory. On a qualification run it is the empty diff or the task's reference fix, which is read from the fixture store (section 6.2). If a scored task has no frozen artifact (the attempt errored before freezing), there is nothing to grade and the outcome comes from the attempt.
2. Create a fresh sandbox at the task's pinned base and pinned image digest, with no network. Record its ID before using it.
3. Load the candidate and re-verify its hash (`gate.Load` for an attempt's artifact; the same envelope and check for a reference fix), then stage it with the exported staging functions.
4. Overlay the fixture from the fixture store. Fixture files replace any file the candidate wrote at the same path, so a candidate cannot pre-empt a hidden test.
5. Run the task's test command. Parse the JUnit report test by test.
6. Compare each test with its expected class (section 9) and record the result.
7. Release the sandbox.

- **Idempotent** on (`run_task` ID, candidate digest). A retry that finds a recorded result returns it.
- **Timeout:** an explicit sum, like `AttemptTimeout`: sandbox creation, staging, the test command's timeout, and a margin. It counts in `LongestSandboxHold`, so the sweep's cutoff covers it (decision 0012).
- **Heartbeats** and cancellation follow `RunAttempt`'s pattern.
- **SWE-bench** tasks use the official harness as the grader behind the same activity interface (section 8).

### 4.4 Throughput and the sweep

- Each attempt worker has one attempt slot (decision 0012). Official runs use several attempt workers on one host, sharing the blob directory.
- **The runner keeps in-flight `BenchTaskWorkflow`s at or below the number of attempt slots.** *Decided.* This closes the sweep problem recorded in 0012's amendment: no attempt row waits on a queue long enough to pass the two-hour cutoff.
- The model rate limit is per worker process. With N attempt workers, each is configured with 1/N of the limit.
- **Open:** how many attempt workers an official run uses. Ticket 3 answers it from ticket 2's measured attempt time.

## 5. Data model and provenance

### 5.1 Dataset manifest

One YAML file per dataset version, in the repository (`benchmarks/<dataset>/<version>.yaml`). *Decided:* versioned by git commit plus a content hash, with three task kinds. The fields are *confirmed* as a starting point.

```yaml
dataset: internal-python
version: 1
tasks:
  - id: py-0001
    kind: public_replay        # public_replay | authored | swebench_verified
    language: python
    repo: example/project      # full name; cloned from the local mirror
    base_sha: <40 hex>
    image_digest: sha256:<...> # overrides the repository's default image
    test_command: [pytest, -q, --junitxml=/tmp/conclave-junit/report.xml]
    spec_path: tasks/py-0001.md        # Task, Acceptance criteria, Hints
    fixture_hash: sha256:<...>         # names the fixture; contents are not in the repo
    reference: {kind: pr, number: 1234, merged_at: 2026-09-14}
```

- The task text follows the owner-section format from the Milestone 1 trials (`docs/trials/README.md`).
- The manifest names a fixture by hash only. Fixture contents live in the fixture store.
- The manifest never replaces `plans.body`, which stays the record of what the agent saw. *Decided.*
- **Freezing.** A dataset version is frozen before any scoring run. A task is never removed from a frozen version because a model failed it; fixing a task means a new version. *Decided.*

### 5.2 Tables

*Decided:* three new tables. The columns are *confirmed* as a starting point, for ticket 3 to settle.

**`benchmark_run`** — one row per run.

| Column | Notes |
|---|---|
| `id` | UUID |
| `kind` | `scoring`, `development` or `qualification` |
| `dataset`, `dataset_version`, `manifest_hash` | What was run |
| `snapshot`, `snapshot_hash` | The run snapshot, canonical JSON, and its SHA-256. Write-once |
| `seeds` | Seeds per task |
| `budget_usd` | The run's cap |
| `state`, `started_at`, `finished_at` | Compare-and-set, as elsewhere (RL-13) |

**`run_task`** — one row per (run, task, seed). The intent row and the result row are the same row.

| Column | Notes |
|---|---|
| `id` | UUID |
| `run_id`, `task_id`, `seed` | Unique together |
| `workflow_id` | Deterministic, set when the row is created |
| `feature_request_id` | Links to the Milestone 1 rows, and through them to attempts, gate results, cost events and tool calls |
| `candidate_digest` | The graded artifact's hash. Write-once |
| `gate_passed` | Whether `TaskWorkflow` passed the Milestone 1 gate |
| `outcome` | Section 9's taxonomy |
| `diagnostic` | True when the gate failed and Grade ran for diagnosis only (0017) |
| `grade_sandbox_id` | Write-once; lets the sweep find a Grade sandbox |
| `grade_report_hash` | Names the per-test result file in the evaluator's store |
| `resolution_passed`, `resolution_total`, `maintenance_passed`, `maintenance_total` | Counts only; test names stay in the report file |
| `state`, `started_at`, `finished_at` | |

**`fixture`** — one row per fixture version.

| Column | Notes |
|---|---|
| `hash` | Primary key; names the fixture in the fixture store |
| `task_id`, `dataset`, `source` | `source` is `pr_tests`, `test_writer` or `swebench` |
| `expected` | Per-test expected class, as JSON |
| `reference_hash` | Names the reference fix in the fixture store. Write-once |
| `author_attempt_id` | The `test_writer` attempt, when there is one |
| `qualification_run_id` | The `benchmark_run` of kind `qualification` that qualified it |
| `signed_off_by`, `signed_off_at` | The owner's authenticated login and the time. Null until signed off |

- `benchmark_run.snapshot`, `run_task.candidate_digest` and `fixture` rows are write-once, enforced the way `artifact_hash` is.
- Each table records the database role that wrote it, as `state_transitions` does (decision 0002).

### 5.3 Run snapshot

One canonical JSON document per run, stored with its hash. *Decided* contents:

- models, and the implementer's effort;
- prompt versions and hashes for each role;
- the SHA-256 of each file in the benchmark deploy set;
- image digests for every task;
- the runner's commit and the workers' build version;
- the rerun policy (qualification and scoring counts);
- the manifest hash and seeds per task.

*Proposed additions:* whether thinking blocks were replayed, the adapter's API version header, and the number of attempt workers.

### 5.4 Result identity

*Decided:* a result binds the candidate digest, not only the run and task. Two consequences:

- A result can always be re-derived: the same digest, fixture hash and image digest must grade the same, apart from flaky tests.
- **A seed is a repetition index, not a sampling seed.** Current models accept no sampling parameters, so nothing makes a seed reproducible. Seeds exist to measure variance.

## 6. Fixtures and hidden tests

Decision 0017 is the authority. This section adds the lifecycle.

### 6.1 Where a task's correctness comes from

| Task kind | Fixture | Reference fix |
|---|---|---|
| `public_replay` | The tests the pull request added or changed | The pull request's diff |
| `authored` | Drafted by `test_writer`, blind | The owner's original code |
| `swebench_verified` | The benchmark's official tests | The gold patch |

### 6.2 Fixture lifecycle

1. **Draft.** For authored tasks, `test_writer` runs once on the evaluator worker, in a sandbox at the base commit, with the task spec and repository profile as input. For public pull requests, a script extracts the pull request's test files.
2. **Qualify.** A `benchmark_run` of kind `qualification` grades two candidates, five times each by default:
   - the empty diff: every resolution test must fail, and every maintenance test must pass;
   - the reference diff: every test must pass.
   A test that flips between reruns disqualifies the fixture as flaky. A draft that touches non-test files is rejected.

   The reference diff is as secret as the fixture: an implementer that saw it would have the answer. It is stored in the fixture store under its own hash, recorded on the `fixture` row, and never written to the attempt blob directory.
3. **Sign off.** The owner reads the fixture and the task spec side by side and checks that the tests call only what the spec names. `conclave bench sign-off <fixture>` records the owner's login.
4. **Freeze.** The fixture hash goes in the manifest. From here the fixture changes only by a new dataset version.

A task is eligible for scoring only when its fixture is qualified and signed off.

### 6.3 Isolation

*Decided* in 0017, which lists the attack tests. In summary:

- Fixtures and reference fixes live in the fixture store, a blob directory only the evaluator worker is given.
- Fixture content never enters an implementer's container, `plans.body`, attempt records, logs or Temporal history.
- `SessionInput.PriorFailure` stays unset. A test fails if Grade output ever reaches `SessionInput`.
- Ticket 5 writes one attack test per path, each using a canary.

## 7. Languages

*Decided:* the grader reads JUnit XML only. A language is a sandbox image plus a test command.

| Order | Language | Test runner and report (*proposed*) |
|---|---|---|
| 1 | Python | pytest, `--junitxml` |
| 2 | JS/TS | The repository's runner with its JUnit reporter |
| 3 | Java | Maven Surefire or Gradle, which write JUnit XML already |

*Decided, 2026-11-26:* Milestone 2 covers these three languages. Go and C++ are deferred to a later dataset version; nothing in the grader is specific to a language, so adding one later is an image, a test command and tasks.

- **Dependencies are baked into the image.** Grading and attempts both run without network. *Decided.*
- **Images are pinned by digest** and built in CI before use (FR-SEC30, decision 0011's amendment). The image build pipeline is therefore a prerequisite of ticket 6, not part of it.
- If a repository's runner cannot write JUnit XML, its reporter is installed in the image, never added to the platform's own module.
- The outline cites a separate "Multi-Language Dataset Plan" for per-language detail. It is not in this repository. **Open:** bring it in, or fold its content into ticket 6.

## 8. Datasets

### 8.1 Internal tasks

Task counts, for the three languages in scope:

| Source | Python | JS/TS | Java | Total |
|---|---|---|---|---|
| Recent public pull requests | 6 | 6 | 6 | 18 |
| Authored tasks | 4 | 4 | 10 | 18 |
| Total | 10 | 10 | 16 | 36 |

The three languages are *decided*. The counts are *confirmed*: the outline had 4 public pull requests per language and 2, 1–2 and 10 authored tasks, which leaves 25 or 26 tasks once Go and C++ are deferred. Raising them keeps the set near its original size and brings the ETS share from about 40% to under 30%. Public pull requests are the cheaper tasks to add, because their tests already exist.

**Public pull requests** qualify when all of these hold (*decided*):

- merged on or after 2026-09-01, which is provisional until Opus 5.5's training cutoff is confirmed;
- linked to an issue, whose text becomes the task;
- adds or changes tests;
- small to medium in size;
- from a repository not already in SWE-bench.

*Confirmed* additions: the repository's licence permits local use, the tests run without network or external services, and the pull request's diff fits the Milestone 1 size caps, so a correct answer is not refused by the gate.

**Authored tasks** are made three ways: reverting a feature, injecting a bug, or writing new work with acceptance tests. *Decided:*

- The ten Milestone 1 trial issues on ETS (base `9839fab`) are the first Java tasks. All ten are included, the unresolved one too, to avoid survivor bias.
- ETS is private, so these tasks and their fixtures stay in git-ignored local files, as the trial issues do today.
- The two Go tasks from Conclave itself are deferred with Go.

**One caution on the Java set.** Ten of the sixteen Java tasks come from one repository, and the implementer prompt was revised during those same trials (`implementer-v2`). Results on them say less about Java in general than the count suggests. The report breaks them out (section 10).

### 8.2 SWE-bench Verified subset

*Decided:*

- A fixed list of 25 instances, with the benchmark version recorded.
- The official per-instance images, pinned by digest, as the sandbox image.
- The official harness as the grader, behind the Grade interface.
- A conformance check per instance: the gold patch resolves it, and the empty patch does not.
- Reported separately, with contamination and test-quality caveats stated.
- *Decided, 2026-11-26:* one seed per model on official runs, to save cost. Its own `benchmark_run` records `seeds: 1`, and the report gives no seed-variance figure for it.

*Confirmed:* choose the 25 by a recorded random draw from instances whose images build and pass conformance, rather than by hand, so the choice cannot favour either model.

## 9. Grading and outcomes

### 9.1 Test classes

Every fixture test has an expected class (*decided*):

| Class | Meaning | On the base | With a correct fix |
|---|---|---|---|
| Resolution | Fail to pass | Fails | Passes |
| Maintenance | Pass to pass | Passes | Passes |
| Setup check | Proves the environment works | Passes | Passes |

Grade passes when every resolution and maintenance test passes. A failing setup check means the environment is broken, not the candidate.

### 9.2 Flaky tests

*Decided:*

- **At qualification,** each candidate is graded five times. A test that flips is flaky, and its fixture does not qualify.
- **At scoring,** a failing grade is rerun up to three times, for diagnosis only. Reruns classify instability; they never turn a failure into a pass.
- Both counts are configurable and recorded in the run snapshot.
- A health check tracks when flips first appear and recommends raising or lowering the counts once enough flake events exist. A change applies only to a new dataset or run version, with the owner's approval.

This is how the benchmark meets FR-A4: an attempt is not blamed for a test that also fails or flips on the unmodified base.

### 9.3 Outcomes

One outcome per `run_task`. Precedence runs top to bottom: the first row that applies wins.

| Outcome | When | In the resolve-rate denominator |
|---|---|---|
| `provenance_invalid` | A recorded hash does not match: artifact, fixture, image or snapshot | No |
| `fixture_invalid` | A setup check fails, or the fixture fails conformance during the run | No |
| `excluded_flaky` | A diagnostic rerun shows the failing tests flip | No |
| `runner_interrupted` | The run was stopped before the task finished and was not resumed, or the run-wide budget cap stopped it | No |
| `cancelled` | The owner cancelled the task | No |
| `infra_error` | The attempt or Grade ended `errored` for a platform reason: sandbox, provider outage | No |
| `budget_exhausted` | The attempt's own token budget ran out. That is the model's result on the task | Yes |
| `refused` | The provider refused (`ReasonRefused`) | Yes |
| `resolved` | The gate passed and Grade passed | Yes |
| `unresolved` | Anything else, including a gate failure | Yes |

- `refused` and the gate-failure rule are *decided*. The denominator column for the other rows is *confirmed*.
- Excluded tasks are never silent. The report lists every one with its reason, and gives the resolve rate both with and without `infra_error` tasks counted as failures.
- `infra_error` tasks may be re-run under a new `run_task` seed slot only by an explicit runner command, and the report says how many were.

## 10. Metrics and report

### 10.1 Metrics

| Metric | Definition |
|---|---|
| Resolve rate | Resolved `run_task`s over eligible `run_task`s, pooled across seeds. *Decided:* 1 seed in development, 3 on official runs |
| Interval | The headline interval is a bootstrap that resamples tasks, keeping each task's seeds together (*confirmed*). A Wilson interval on the pooled proportion is shown beside it (*decided*), labelled as optimistic: it treats the seeds of one task as independent, so it is narrower than the data supports |
| Paired comparison | Between two runs on the same frozen tasks: per-task flips, and a bootstrap interval on the difference in resolve rate, resampling tasks. *Decided.* A comparison reuses a stored run as its baseline when the dataset version, fixtures, image digests and everything in the run snapshot except the change under test match. Only the new arm runs. A baseline is rerun only when one of those has changed (*decided*, 2026-11-26) |
| Cost per resolved task | Total cost of eligible `run_task`s over resolved ones. Fixture generation cost is tracked separately. *Decided* |
| Time per task | Median and P95 from attempt start to gate result. Batch makespan is reported separately. *Decided* |
| Turn latency | Per model call, by model and effort, to revise `MaxWallClock` (decision 0016) |
| Gating cost | `clean_room` vs. `reset_in_place` on the same artifacts (FR-SEC27) |
| Refusal rate | By model and category |

- Breakdowns by dataset, language and task kind. The ETS tasks are also shown apart from the other Java tasks.
- **A stated precision limit.** With 36 internal tasks, a resolve-rate interval is wide (roughly ±16 points at 50%), and per-language numbers are descriptive only. The report says so on its first page.

### 10.2 Report

- *Decided:* a JSON file rebuilt only from stored records, starting from `trial_report.sql`. Counts must reconcile with `run_task` rows, and the generator fails if they do not.
- *Proposed:* `conclave bench report <run>` also prints a short Markdown summary from that JSON. Nothing in it is computed anywhere else.
- The report carries the run snapshot hash, the manifest hash and the list of exclusions.

### 10.3 Measurements owed to earlier decisions

| Owed | How Milestone 2 measures it |
|---|---|
| Implementer effort (outline, 2026-11-24) | Ticket 2 runs its tasks at two effort levels on both models and chooses the lowest level that holds the resolve rate, so the choice weighs cost as well as quality (*decided*, 2026-11-26). The value is fixed before any official run |
| Task budget | Ticket 2 records tokens per passing attempt and sets the `task_budget` value from them (section 10.4) |
| `MaxWallClock` (0016) | Turn latency and wall-clock stops from the development runs |
| FR-SEC27 default | After a scoring run, re-gate a sample of its frozen artifacts in both modes and compare time and result. No model calls |
| Run budget cap | Set after ticket 2 measures cost per task |

### 10.4 Cost

*Decided, 2026-11-26:* the cost controls below. Prompt caching and the task-budget plumbing land in ticket 1, before ticket 2 measures anything. The other controls are in sections 8.2 (one SWE-bench seed), 10.1 (baselines reused) and 10.3 (effort chosen for cost).

**Prompt caching.** The Anthropic adapter today sends every request uncached (decision 0008, item 5). In the Milestone 1 trials, input was 65% of the implementer's cost, because each step resends the whole conversation.
- The adapter marks the end of the system prompt and tools with an explicit cache breakpoint, and sets top-level automatic caching for the growing conversation. This is the pattern the API documents for agent loops.
- Caching does not change what the model outputs, so cached and uncached attempts are comparable. The agent loop's history only grows, which is what caching needs, and it fits thinking replay.
- The router config already holds cache write and read prices, and the adapter already reads the cache usage fields, so cost accounting needs no change.
- **TTL:** the 5-minute default. A cache read refreshes it, but the clock runs from each request's start, so a long model turn followed by a slow `run_tests` can let it lapse. Ticket 2 counts cache writes after the first step. If lapses are common, the 1-hour TTL (2× the write price) is set in the deploy set and recorded in the run snapshot.
- This is an adapter change with no contract change. It needs a dated amendment to decision 0008.

**Haiku 5.5 for development passes.** Development runs check the pipeline, not the model: datasets load, Grade runs, rows reconcile. They run the implementer on `claude-haiku-5-5`.
- Development results are never reported as benchmark results, and never compared with official ones. The run snapshot records the model, so the two cannot be confused.
- Opus stays for ticket 2's effort measurement, for `test_writer` and for official runs.
- **Haiku 5.5 has two rate cards:** $0.10 / $0.50 per million tokens when the prompt is 100K tokens or fewer, and $0.50 / $2.50 when it is longer. The router holds one price per model, so the benchmark deploy set prices Haiku at the higher card. Cost and budget caps then never undercount; they overcount short prompts. Its cache prices follow the same card: reads at 0.1× and 5-minute writes at 1.25× the input rate.

**Task budget.** The API's `task_budget` (beta `task-budgets-2026-03-13`, in `output_config`) tells the model how many tokens it has for the whole loop, and shows it a countdown. It is advisory, unlike `max_tokens`, and is meant to make an attempt wrap up instead of running into the wall clock. The two Milestone 1 wall-clock stops cost $12.97 and delivered nothing.
- It changes how the agent works, so it applies to production attempts as well as benchmark ones, through the same adapter and role configuration. Otherwise the benchmark would measure an agent that production does not run.
- Its value is not guessed. Ticket 2 measures tokens per passing attempt without it and sets the value from that, with a margin. The minimum is 20,000 tokens. Attempts after that carry the value in their provenance, and the run snapshot records it.
- The budget leaves `remaining` unset: the API tracks the countdown from the history the loop resends, and the loop never rewrites that history.
- It is a request field and a configured value per role. It needs no contract change, but it does need an entry in decision 0008's amendment.

**A clean Haiku pass before every official run.** An official run starts only after a Haiku development pass over the same frozen dataset versions ends with no `infra_error`, `fixture_invalid` or `provenance_invalid` rows. Infrastructure faults are then found at Haiku prices, not Opus ones.

**Estimated cost** (extrapolated from the ten Milestone 1 trials on Fable 5.2, so a direction, not a budget; ticket 2 replaces it with measured cost per task). It does not count savings from a lower effort or the task budget, which ticket 2 measures:

| Item | Before | With these controls |
|---|---|---|
| Ticket 2's effort measurement | ~$35 | ~$20 |
| Development passes | ~$700–1,000 | ~$15–55 |
| `test_writer` fixtures | ~$100 | ~$50 |
| One official run (36 internal tasks × 3 seeds + 25 SWE-bench × 1 seed, × 2 models) | ~$1,050 | ~$340 |
| **Milestone 2 total** | **~$2,000–2,300** | **~$430–470** |

The local router's `monthly_usd` cap is $200. An official run needs that raised for its month, or a run budget of its own (open item 4).

## 11. Security work in this milestone

- **Hidden-test isolation.** Section 6.3 and ticket 5.
- **Canary detector (FR-SEC20).** Decision 0016 placed it here. Canaries are planted in fixtures and benchmark environments; a canary in a prompt, log, artifact or record halts the run and raises the `halt` alert. The isolation tests in ticket 5 are its first users, so, *confirmed:* build the detector in ticket 5.
- **Benchmark repositories are untrusted input.** Public repositories and issue text are third-party content. Issue text is quarantined exactly as in production (FR-SEC28), and the sandbox rules do not relax for the benchmark: no network in the agent phase, no worker environment, no Docker socket.
- **Tier.** Benchmark repositories of public code are configured `public`. ETS tasks keep ETS's tier.
- **Credentials.** The benchmark attempt worker holds model credentials and nothing for GitHub.

## 12. Contract and platform changes

Contracts are reviewed by hand. These are the changes this milestone asks for, gathered for ticket 1:

| Change | Why | Decided in |
|---|---|---|
| `contracts.Message`: an opaque, adapter-owned field holding the assistant turn's content as received | Replay thinking blocks in their original order | Outline, 2026-11-26 |
| `contracts.Provenance`: an effort field; `attempts.effort` column | Record the effort each attempt ran at | Outline, 2026-11-24 |
| `StopRefusal` stop reason, `ReasonRefused` failure reason, and a place for the refusal category | A refusal is an outcome, not a provider error | Outline, 2026-11-26 |
| `MaxChangedFiles` and `MaxDiffBytes` on `contracts.Constraints` | The implementer is told one of its three size caps (decision 0016, known gap). A benchmark task would fail on a cap the agent never saw | *Confirmed*, 2026-11-26 |

Platform changes outside contracts:

- `llm.RoleRoute` gains `effort`; the adapter sends it and replays thinking blocks.
- The adapter turns on prompt caching and sends a `task_budget` from the role's configuration (section 10.4), with a dated amendment to decision 0008.
- A refused attempt ends `failed/refused` and gets no retry ordinal. This changes `TaskWorkflow`'s routing, so it is gated with `workflow.GetVersion` and the replay fixtures must still pass.
- `internal/gate` exports staging and gains a per-test JUnit parser. `gate.Input` is unchanged.
- `sandboxes.yaml` accepts a per-task image.
- A test fails if the agent loop ever edits or removes an earlier message.

Each of the adapter changes alters model behaviour, so they land before ticket 2 measures anything, and a prompt or adapter version bump marks attempts made after them.

## 13. Ticket plan

One ticket per session, as in Milestone 1. Each ticket's own file will name its requirements, tests and review-list items; this is the outline of the work.

| # | Ticket | Depends on | Review list |
|---|---|---|---|
| 0 | `/continue` command (already written: [`continue-command.md`](continue-command.md)) | — | RL-9 |
| 1 | Close-out and contracts: accept 0010, 0013, 0014; the four contract changes; effort, thinking replay, prompt caching, task budget and refusals in the adapter and workflow | 0 | RL-6, RL-16 |
| 2 | Vertical slice: minimal runner, evaluator worker, Grade, three hand-written Python tasks; the effort measurement | 1 | RL-2, RL-3, RL-14 |
| 3 | Persistence and operations: the three tables, run snapshot, intent rows and reattachment, in-flight bound, benchmark worker without GitHub, several attempt workers | 2 | RL-2, RL-4, RL-13 |
| 4 | Fixture lifecycle: `test_writer` role, qualification runs, owner sign-off | 3 | RL-11 |
| 5 | Isolation attack tests and the canary detector | 4 | RL-20 |
| 6a | Image build in CI (FR-SEC30) | — | RL-5 |
| 6b–6d | Datasets, one ticket per language: Python, JS/TS, Java | 4, 6a | — |
| 7 | SWE-bench Verified subset | 3, 6a | — |
| 8 | Seeds, the comparison report with baseline reuse, the owed measurements, the clean-Haiku-pass entry check, exit checks | 5, 6, 7 | — |

- Splitting ticket 6 by language and pulling the image build out are *confirmed*; the outline has one ticket 6.
- Ticket 2's results are development-only. No number from before ticket 5 is reported as a benchmark result, because the isolation tests do not exist yet.
- Dataset curation (shortlisting repositories, writing authored tasks) is the owner's work and can start now, in parallel with tickets 1 to 3.

## 14. Risks

| Risk | Mitigation |
|---|---|
| Contamination inflates public-PR scores | Only pull requests merged after the tested models' cutoffs; authored tasks as an uncontaminated check |
| The cutoff date is provisional | Confirm Opus 5.5's training cutoff before freezing the first dataset |
| The Java environment eats time | Build it last; prefer small builds |
| Curation takes longer than coding | Shortlist repositories early; about 30 to 60 minutes per authored task |
| Run cost: tasks × 3 seeds × 2 models, plus SWE-bench | Prompt caching; Haiku 5.5 for development passes; a per-run budget cap |
| Wall time: about 44 hours serial for an official run | Several attempt workers; one model or fewer seeds until the pipeline is proven |
| The sweep releases a queued attempt's sandbox | The runner bounds in-flight work to the slot count |
| N workers send N times the rate limit | Each worker gets 1/N of the limit |
| Models compared at different effort | One explicit implementer effort, recorded per attempt |
| Hidden-test output reaches a later attempt | Grade after the task; isolation tests |
| `test_writer` writes weak or wrong tests | Qualification against the reference; owner sign-off |
| Hidden tests call names the spec never states | The sign-off check; the authored-task format states every interface |
| A correct fix exceeds the gate's size caps | Screen public pull requests against the caps; tell the implementer all three caps |
| The security-flavoured tasks trip the provider's classifiers | `refused` is counted and reported by category |
| Small samples read as precise | Intervals on every rate; the precision limit stated up front |
| Ten Java tasks share one repository | Reported apart from the other Java tasks |

## 15. Open items and confirmed proposals

**Open**

1. How many attempt workers an official run uses (ticket 3).
2. The repository shortlist per language, and whether recent pull-request activity is enough to find six per language.
3. The implementer's effort value (ticket 2 measures it).
4. The run budget cap (after ticket 2).
5. Whether the Multi-Language Dataset Plan comes into the repository.
6. Decisions 0010, 0013 and 0014 are still `proposed`.
7. The cache TTL: 5 minutes unless ticket 2 finds lapses (10.4).
8. The `task_budget` value (ticket 2 measures it, 10.4).

**Proposed in this draft and confirmed by the owner, 2026-11-26**

1. `BenchTaskWorkflow` wraps the task and its grade (4.2).
2. The table columns (5.2) and manifest fields (5.1).
3. Qualification is a `benchmark_run` of its own kind, graded by the same Grade, with reference fixes kept in the fixture store (4.3, 5.2, 6.2).
4. The denominator rule for each outcome, with a run-wide budget stop recorded as `runner_interrupted` (9.3).
5. A task-level bootstrap interval as the headline, with the Wilson interval beside it (10.1).
6. The SWE-bench subset is a recorded random draw (8.2).
7. The extra public-PR criteria: licence, no network in tests, fits the size caps (8.1).
8. All three size caps go on `contracts.Constraints` (12).
9. The canary detector is built in ticket 5 (11).
10. Ticket 6 splits by language (Python, JS/TS, Java), with the image build as its own ticket (13).
11. The requirement rewording in section 2.
12. The task counts: 6 public pull requests per language, and 4 authored tasks each for Python and JS/TS, for 36 in all (8.1).

A confirmed proposal that changes a recorded decision gets a decision record, starting at 0018.

**Still proposed, minor:** the extra run-snapshot fields (5.3), the per-language test-runner table (7), and the Markdown summary of the report (10.2).
