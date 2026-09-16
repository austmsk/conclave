# Agent Roles and Contracts (Draft v0.1)

Requirement IDs refer to `scope-and-requirements.md`. Review-list IDs refer to `review-list.md`.

Milestone 1 implements the **implementer** role only. Every other role is sketched here so that adding it later requires new values, not new machinery.

## The Shared Shape

A role is data, not code. One agent loop executes all roles. A role is defined by:

| Field | Meaning |
|---|---|
| `Role` | Stable identifier (`implementer`, `judge`, …) used by the router, the access matrix, and cost records |
| `PromptVersion` | Version string for the role's static instructions; recorded on every output |
| `Tools` | Explicit allowlist. Absent means denied (FR-SEC3, RL-11) |
| `InputSchema` | Typed input the platform assembles; agents never fetch their own context |
| `OutputSchema` | Typed output, validated before use |
| `Limits` | Steps, tokens, wall clock, repeat-error circuit breaker |

Rules that apply to every role:

- **Context is supplied, never fetched.** The platform assembles the input. An agent cannot decide to read something outside its tool allowlist.
- **Untrusted text is quarantined.** Issue text, repository files, and comments arrive inside nonce-delimited blocks with provenance labels (FR-SEC28). They are never part of the role's static instructions (FR-SEC6).
- **Output is validated.** Schema mismatch triggers one repair attempt with the validation error included; a second failure fails the attempt with reason `invalid_output`, tracked separately in evaluation reports.
- **Provenance is recorded.** Every output records role, prompt version, exact model ID, token counts, and cost.
- **Limits are enforced by the loop**, not requested in the prompt.
- **A circuit breaker stops repetition.** After `MaxRepeatErrors` identical command failures, the session ends and reports what it tried rather than continuing to guess.

## Implementer (Milestone 1, full specification)

**Purpose.** Given a task and a sandboxed working copy, produce a change that satisfies the acceptance criteria, with tests.

**Model.** Router role `implementer`. Milestone 1: Anthropic only. Milestone 3: a pool across providers, one per attempt.

### Input

| Field | Notes |
|---|---|
| `TaskSpec` | Description and explicit acceptance criteria |
| `IssueText` | Quarantined, provenance-labelled |
| `RepoProfile` | Language, layout summary, build/test/lint command names, conventions note. Milestone 3 replaces this with the codebase brief |
| `BaseCommit` | The pinned SHA the sandbox was built from |
| `Constraints` | Diff size cap, protected paths, execution-surface paths, dependency policy |
| `PriorFailure` | On a retry only: the previous attempt's gate failures |

### Excluded from input (enforced now, matters later)

- Hidden tests (FR-A2)
- Any other attempt's work or transcript
- The platform's own repository
- Git history before the base commit
- Substituted fixture files' real contents (FR-SEC22)

### Tools

| Tool | Arguments | Notes |
|---|---|---|
| `read_file` | path, optional range | Path-validated (FR-SEC29) |
| `list_dir` | path | Path-validated |
| `search` | pattern, optional glob | ripgrep; results truncated with a count |
| `write_file` | path, content | Path-validated; rejects `.git` internals and substituted fixtures |
| `apply_patch` | unified diff | Rejects absolute paths and `..` segments |
| `run_tests` | none | Command string comes from repository config, not from the model |
| `run_build` | none | As above |
| `run_lint` | none | As above |
| `run_command` | command, timeout | General escape hatch. Every use is an audit outlier by construction (FR-SEC24) |
| `request_dependency` | package, version, reason | Proposes only; platform code performs checks and the install (FR-SEC14) |
| `finish` | completion report | Explicit completion signal |

No network tool. No git tool. No GitHub tool. Pushing and PR creation are platform actions (FR-SEC5).

### Output

The substantive output is the diff, extracted from the sandbox by platform code. The `finish` call carries the structured report:

| Field | Use |
|---|---|
| `Summary` | PR description |
| `FilesChanged` | Cross-checked against the actual diff; a mismatch flags the attempt |
| `TestsAdded` | Feeds the gate and the judge rubric |
| `Decisions` | Recorded at decision time so `/ask` can cite them (FR-A6) |
| `Uncertainties` | Surfaced in the PR's risk section |
| `DependenciesRequested` | Cross-checked against approved requests |

### Limits (starting values, revised with Milestone 2 data)

| Limit | Value |
|---|---|
| `MaxSteps` | 60 tool calls |
| `MaxTokens` | Per-attempt budget from repository config |
| `MaxWallClock` | 20 minutes |
| `MaxRepeatErrors` | 3 identical command failures |

Exceeding steps, tokens, or wall clock ends the attempt as `failed` with the reason recorded. Infrastructure problems end it as `errored` instead, so evaluation metrics separate agent failure from platform failure.

## Other Roles (sketches)

| Role | Milestone | Input | Tools | Output | Notes |
|---|---|---|---|---|---|
| `brief` | 3 | Repository at a commit | `read_file`, `list_dir`, `search`, LSP queries | Codebase brief: architecture, module map, commands, conventions, key dependencies | Read-only. Cached per commit. Output is untrusted data downstream (FR-SEC6) |
| `planner` | 4 | Issue text, codebase brief, repository constraints | Read-only tools | Task graph with acceptance criteria, predicted file footprints, risk flags, decision records | Read-only. Plan validated mechanically before execution |
| `test_writer` | 2 (start) | Task spec, repository profile | Implementer tools minus `request_dependency` | Visible and hidden test sets | Runs before implementers; hidden set withheld (FR-A2). Ships before any benchmark results are generated |
| `judge` | 3 | Two attempts' diffs and reports, rubric | Read-only tools | Winner, rationale, rubric scores | Provider chosen by the outside-provider rule; presentation order randomized |
| `pr_writer` | 1 (minimal) | Task spec, diff, gate results, decisions | None | PR title, description, risk section | No tools; pure synthesis. Minimal version in Milestone 1 |
| `discussion` | 1 (minimal) | Question, plan, decisions, codebase brief | Read-only tools | Answer citing decisions and files | Never mutates state (FR-S6). The only role permitted on the platform repository before Milestone 2 (FR-SEC32) |
| `conflict_resolver` | 5, conditional | Base, two diffs, both task specs | Implementer tools | Merged diff | Built only if measured rerun costs justify it (FR-S14) |

## Contracts Package Outline

Types the owner reviews by hand before anything is built on them.

```
internal/contracts/
  role.go        Role, RoleSpec, Limits, PromptVersion
  session.go     Session input/output envelope, provenance, usage
  tool.go        ToolName, ToolCall, ToolResult, schema definitions
  task.go        TaskSpec, AcceptanceCriterion, Constraints
  report.go      CompletionReport, Decision, Uncertainty
  attempt.go     Attempt, AttemptState, FailureReason
  gate.go        GateName, GateResult, ArtifactHash
  sandbox.go     Sandbox interface
  llm.go         Router interface, RoleRequest, Response
```

Milestone 1 implements `implementer` and a minimal `pr_writer` and `discussion`. The remaining roles exist as declared values with empty tool lists until their milestone.

## Open Questions

- Whether `write_file` and `apply_patch` should both exist, or only `apply_patch`. Two tools is more forgiving for models; one tool is easier to audit. Decide after the first tickets show which the model actually reaches for.
- Starting limit values, which are guesses until Milestone 2 measures real attempts.

## Decisions

- **Milestone 1 implementers write their own tests.** The walking skeleton stays thin, with no separate test writer. The `test_writer` role and the hidden-test split ship at the start of Milestone 2, before any benchmark results are generated, so evaluation numbers always come from a pipeline that includes tests the implementer never saw.
