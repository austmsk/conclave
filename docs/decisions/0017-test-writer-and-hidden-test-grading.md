# 0017 — Hidden tests are graded after the attempt, and `test_writer` writes benchmark fixtures, not production tests

**Date:** 2026-11-24
**Status:** accepted

## Context

Milestone 2's first success criterion is that "the `test_writer` role and the hidden-test split are in place before any benchmark results are generated". The requirements and Milestone 1's seams describe one way to do that:

- **FR-S7** has a test writer produce tests from the acceptance criteria before implementation begins. Milestone 1 waived this: the implementer writes its own tests.
- **FR-A2** withholds part of the test writer's tests from implementers and applies them "only during gating". Threat T2 is mitigated the same way.
- **Exit-checklist item 5** reserved a seam for that: hidden tests as an overlay in `gate.Gate`, applied after the recorded diff and before the command gates.
- **`docs/agent-roles.md`** has `test_writer` run before the implementers and return a visible and a hidden test set.

Planning the evaluation harness (the M2 design outline, sections 4 and 6) found four problems with running hidden tests inside the gate:

1. **A gate failure starts a new ordinal.** `TaskWorkflow` runs a second attempt after a failed gate. If hidden tests can fail the gate, that second attempt exists because of them. That leaks one bit per task, and it means the benchmark measures a retry loop driven by tests the implementer was never meant to influence.
2. **Gate output goes where agents and owners can read it.** `gate_results` records each gate's failure, and the status comment tells the owner that an attempt "did not pass the gates". `SessionInput.PriorFailure` is defined to carry "the previous attempt's gate failures" to the next attempt. Nothing in production sets it today (`attemptRun.sessionInput` leaves it empty), but if it were wired up later it would send hidden-test names and output straight into the implementer's context.
3. **`reset_in_place` gates in the agent's own container.** That is the FR-SEC27 default. Overlaying hidden tests there writes them into an environment the agent ran in. A process the agent left behind with `run_command` could read them, and if any part of that environment were reused, they could outlive the attempt.
4. **A per-run test writer would unfreeze the benchmark.** If `test_writer` ran before each implementer, every seed would be graded against different tests. Results would then compare the tests as much as the implementers, and no fixture could be checked against a reference fix before it scores anything.

There are therefore two questions: where hidden tests run, and whether `test_writer` joins the production pipeline in Milestone 2. The owner settled the first in the design outline on 2026-11-16 (gate vs. grade). They also settled who writes fixtures: a public PR's own tests are its fixture, SWE-bench's official tests are its fixture, and `test_writer` drafts fixtures for authored tasks only. This record writes those choices down and proposes the second.

## Decision

### Hidden tests run in Grade, after the task, never in the gate

- **The Milestone 1 gate is unchanged.** It runs the repository's visible tests and the static checks: size, protected paths, gate weakening, secrets and the execution surface. It decides whether a candidate could be delivered. `gate.Input` gets no hidden-test field, and exit-checklist item 5's overlay seam is not used.
- **Grade is a separate activity that runs after `TaskWorkflow` completes.** It grades the final ordinal's frozen artifact, named by `attempts.artifact_hash`, in these steps:
  1. Build a fresh sandbox at the pinned base and the pinned image digest. This is always clean-room, whatever the repository's FR-SEC27 mode.
  2. Load the artifact with `gate.Load`, which re-verifies the hash, and stage it.
  3. Overlay the fixture and run the task's test command with no network.
  4. Parse the JUnit report test by test, so fail-to-pass and pass-to-pass tests are scored apart.

  Grade records its own sandbox ID and releases the sandbox in a deferred, disconnected context. It is idempotent on (run task, candidate digest).
- **Grade output never reaches an attempt.** Grade results go only to the evaluation tables. They are never written to `attempts`, `gate_results`, `tool_calls` or `plans.body`, and they never cause a new ordinal. Activities pass IDs and counts only (RL-14), so test names and output stay out of Temporal history.
- **Only one process can read fixtures.** The fixture store is a blob directory configured only on an evaluator worker, which polls its own task queue. The benchmark's attempt worker is never given that directory. Grade and the `test_writer` sessions both run on the evaluator worker.
- **Resolved means both checks pass.** A task is `resolved` when its `TaskWorkflow` passed the gate and Grade passes every resolution and maintenance test. A task whose gate failed is `unresolved`. Grade still runs on its last frozen artifact, and that result is kept for diagnosis only. It is recorded apart from the score, marked as diagnostic, and never counts toward resolve rate or cost per resolved task. It answers a different question: whether the hidden tests would have passed a candidate that the gate refused.

### `test_writer` writes fixtures at dataset build time, for authored tasks only

- **It runs once per task, while the dataset is built.** It does not run once per attempt, and it never runs inside `TaskWorkflow`. A seed's score depends on a frozen fixture, not on what the test writer produced that day.
- **It works blind.** Its input is the task spec and the repository profile at the base commit, the same fields the implementer gets. It never sees the reference fix, any candidate, or another task's fixture. Its sandbox is built at the base, so the fix is not in its tree.
- **It produces only a hidden set.** In Milestone 2 there is no visible test-writer set. The implementer's visible tests are the repository's own tests plus whatever the implementer writes, as in Milestone 1. That keeps `TaskWorkflow` exactly what Milestone 1 measured.
- **Its draft is qualified, not gated.** The draft fails on the base by design, so the Milestone 1 gate would reject it. Instead, qualification is a set of Grade runs:
  - With the empty diff, every resolution test fails.
  - With the reference diff, every resolution test passes and the existing tests still pass.
  - Each result holds across the qualification reruns (five by default).

  A draft that changes non-test files is rejected. The owner's sign-off is the last step before a task is eligible. A fixture changes only by making a new dataset version.
- **The role ships through its seams.** That means a `RoleSpec` and prompt (`test-writer-v1`), a router route and a `repos.yaml` allowlist entry for the benchmark repositories. The role is added to the evaluator worker's role map. The production worker's `roles()` is unchanged, and `TestRolesAreMilestoneOne` keeps holding for it.

### Production pipeline: unchanged in Milestone 2

- **Labelled issues run exactly as in Milestone 1.** The implementer writes its own tests, and no hidden tests apply. The FR-S7 exception extends through Milestone 2.
- **A later decision puts `test_writer` into production**, using Milestone 2's data. When it does, it keeps the rules this record sets for hidden tests:
  - they run after the gate, in a fresh container;
  - a failure goes to `needs_attention`, never to a new ordinal;
  - their output never enters `SessionInput`.

  The benchmark then measures that pipeline as a new run version, paired against this one.

### Isolation is tested by attempting the leak

Ticket 5 of the M2 plan writes these as attack tests, beside the code they guard:

| Path | The test shows |
|---|---|
| Implementer container | A fixture's canary content is absent from the sandbox's filesystem, mounts and environment during and after an attempt |
| Agent context | `SessionInput` is built from `plans.body` only. A Grade result with a canary test name, written before a second ordinal starts, does not appear in that ordinal's `SessionInput` or prompt |
| Records | A Grade canary appears in no `attempts`, `gate_results`, `tool_calls` or `plans` row, and in no Temporal history |
| Logs | A Grade canary is absent from the attempt worker's log output and its blob directory |
| Process boundary | The attempt worker starts with no fixture-store configuration. Setting one is a startup error |

## Alternatives considered

- **Hidden tests as a gate overlay**, as exit-checklist item 5 planned. Rejected for problems 1 to 3 above. The overlay seam was additive, so leaving it unused costs nothing.
- **Grade inside `TaskWorkflow`, after the gate.** Rejected: the attempt worker would need the fixture store, and the workflow Milestone 1 measured would change. Running Grade on the evaluator worker keeps the trust boundary at a process.
- **`test_writer` before every implementer attempt, as `agent-roles.md` describes.** Rejected for Milestone 2 by problem 4. This is the production shape that a later decision may still choose, subject to the rules above.
- **`test_writer` for every task kind.** Rejected by the owner: public PRs and SWE-bench already come with tests a human wrote against a known fix, which are a stronger reference than a model's draft.

## Consequences

- **Scoring a task always takes two steps.** The gate answers "would this be delivered?", and Grade answers "is it correct?". The report counts a task as resolved only when both pass, so the score never credits a candidate that production would have refused.
- **Grade holds a sandbox.** Its timeout counts in `LongestSandboxHold`, so the sweep's cutoff covers it (0012).
- **Hidden tests need names the task spec states.** A resolution test that calls a function the task spec never names fails every honest candidate. Qualification catches this only partly, because the reference fix knows the name. The owner's sign-off checks for it, and the authored-task format states every interface the fixture calls.
- **Fixture text exists in the evaluator's records.** A `test_writer` session records its tool calls like any other, so `tool_calls.arguments_raw` and the evaluator's transcript blobs hold the fixture. Nothing on the attempt path reads another attempt's tool calls, and the isolation tests above check that. Anyone who can read the evaluator's records can read the fixtures: that is the owner.
- **The Milestone 2 report states what it measured.** The pipeline has no test writer, and Grade checks correctness only after the task. Milestone 3's best-of-N comparison inherits the same Grade step.

### Changes on acceptance

Applied when the owner accepted this record, on 2026-11-24:

| Document | Change |
|---|---|
| `docs/scope-and-requirements.md`, FR-A2 | "A portion of the test writer's tests is withheld from implementers and applied only during gating" becomes "Hidden tests are withheld from implementers and applied only when grading, in a fresh sandbox after the task completes. Their results never reach an implementer or start a new attempt." |
| `docs/scope-and-requirements.md`, FR-S7 | The Milestone 1 exception becomes "Milestones 1 and 2", and the clause about the hidden-test split points to FR-A2 |
| `docs/security-and-threat-model.md`, T2 | The mitigation becomes "Hidden tests stored in an evaluator-only store, applied only when grading in a fresh sandbox; isolation attack tests", still citing FR-A2 |
| `docs/agent-roles.md` | The `test_writer` row: runs at dataset build time on authored tasks, produces a hidden set, qualified against the reference. The closing decision points here |
| `docs/tickets/m1-tickets/m1-exit-checklist.md`, item 5 | A dated note: the overlay seam is not used, and hidden tests are graded after the task (0017) |
| `docs/decisions/README.md` | Status `accepted` |
