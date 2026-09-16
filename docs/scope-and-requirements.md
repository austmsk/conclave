# Scope and Requirements (Draft v0.1)

## Purpose

A single-owner, self-hosted platform that takes GitHub issues on the owner's repositories, understands the existing codebase, plans the change, produces and tests competing implementations in isolated sandboxes, selects and integrates the best result, and opens pull requests for human review.

Every requirement has an ID (for example `FR-S2`). Tickets, tests, and success criteria reference these IDs so each piece of work traces back to a requirement.

## In Scope

- The owner's own repositories that have automated builds and tests
- Initial languages: Java, C++, TypeScript (Next.js and Payload CMS), Python
- Both existing codebases and new codebases
- Work triggered and controlled through GitHub issues, comments, and pull requests
- Question-and-answer discussion with the platform about plans, trade-offs, and decisions

## Non-Goals

- No web dashboard; GitHub and generated static reports are the only interfaces
- No multi-tenancy and no users other than the owner
- No autonomous feature ideation in v1
- No Unity or other game-engine repositories
- No auto-merge; the owner always merges
- No deployments, and no access to production data, databases, or credentials
- No self-hosted microVM infrastructure in v1
- No IDE plugin or standalone chat application
- No MCP servers in the agent tool set. Agent tools are a small, audited set of Go functions executed by the worker. MCP servers hold their own credentials and network access, which would place capabilities on the agent side of the sandbox boundary, and would add tool-poisoning and rug-pull risks the platform does not otherwise have. See `security-and-threat-model.md` for the conditions under which MCP could be reconsidered.

## Success Criteria by Milestone

**Milestone 1: Walking skeleton**
- A labeled issue on the Election Tally repository produces a PR that passes the repository's CI in at least 7 of 10 trials
- Zero orphaned sandboxes after 10 runs, including at least 2 cancelled runs
- Every run is traceable end to end in the Temporal UI and the database

**Milestone 2: Evaluation harness**
- The `test_writer` role and the hidden-test split are in place before any benchmark results are generated
- A replay benchmark of at least 20 tasks built from the owner's repositories, plus a SWE-bench Verified subset
- A generated report showing resolve rate, cost per resolved task, and time per task
- Every result records model versions, prompt versions, and configuration; flaky tests are detected and excluded

**Milestone 3: Competition and judging**
- Best-of-N with pairwise judging is measured against the single-agent baseline on the benchmark, with cost reported
- Success means the comparison is measured and documented, not that a particular outcome occurs
- At least three model providers are routed by role

**Milestone 4: Planning, integration, distribution**
- A multi-task feature is planned into a task graph, executed sequentially (`max_parallel_tasks: 1`) by the general scheduler, integrated through the merge queue, and delivered as stacked PRs that the owner merges
- Every task records its actual duration and its predicted versus actual changed files, so footprint-prediction accuracy is measurable before parallel execution exists
- Workers run on at least two machines

**Milestone 5 (stretch): Parallel tasks**
- The same scheduler runs with a limit greater than 1, using file-footprint overlap to serialize tasks that would edit the same files
- The automatic parallelism decision (FR-S13) and downshift rule (FR-S13a) are implemented
- The same set of multi-task features is run always-sequential, always-parallel, and with the automatic decision, with a report comparing wall-clock time, cost, conflict rate, and task reruns
- Success means the comparison is measured and documented, and the default setting is chosen based on the results

## Functional Requirements

Grouping follows the ISO/IEC 9126 functionality sub-characteristics: suitability, accuracy, interoperability, security, and compliance. (In ISO/IEC 25010, the successor standard, security is classified as its own quality characteristic.)

### Suitability: the system does the right things

- **FR-S1 Trigger.** A run starts when the owner applies the `agent` label to an issue in an enabled repository.
- **FR-S2 Existing-code understanding.** Before planning in an existing codebase, the platform produces a codebase brief pinned to the base commit. The brief covers: architecture overview, module map, build/test/lint commands, conventions (naming, error handling, testing style, directory structure), key dependencies, and the existing code most relevant to the request.
- **FR-S3 Plans build on existing code.** Every planned task names the existing files and symbols it modifies or extends and the existing patterns it follows. Any new file must include a justification for why existing code cannot be extended.
- **FR-S4 New codebases.** When a repository has little or no code, the platform skips convention extraction; the plan includes a scaffolding task and proposes conventions for owner approval.
- **FR-S5 Plan approval.** No implementation starts before `/approve`. `/revise <feedback>` produces a new plan version.
- **FR-S6 Discussion.** `/ask <question>` on an issue or PR returns an answer grounded in the codebase brief, the plan, and recorded decisions. `/ask` never changes any state.
- **FR-S7 Tests first.** Every behavior change includes tests. A test writer produces tests from acceptance criteria before implementation begins. Milestone 1 exception: the implementer writes its own tests; the separate test writer and the hidden-test split ship at the start of Milestone 2, before any benchmark results are generated.
- **FR-S8 Competing attempts.** The number of attempts per task is configurable.
- **FR-S9 Integration.** Winning attempts land through a merge queue, with the full test suite rerun after each landing.
- **FR-S10 Pull requests.** Each PR description covers what changed and why, links the approved plan, includes test evidence, lists risks, and summarizes key decisions. PRs above the size cap are split into stacked PRs.
- **FR-S11 Control commands.** `/status`, `/retry`, `/resume`, `/cancel`, and `/help` are supported.
- **FR-S12 Escalation.** When the platform is stuck (no attempt passes, a conflict cannot be resolved, or a budget cap is hit), it pauses and posts a comment explaining what happened and the available options. It never fails silently.
- **FR-S13 Task concurrency.** Each repository sets a maximum (`max_parallel_tasks`, default 1). For each run, a deterministic function chooses the actual limit after planning, using task-graph width after footprint serialization, estimated time saved (total duration versus critical path), measured footprint-prediction accuracy for the repository, and plan risk flags. Repositories without accuracy history run sequentially. The choice and its reason appear in the plan comment and are stored as a decision record; the owner can override with `/approve sequential` or `/approve parallel`. Sequential and parallel execution use the same scheduler.
- **FR-S13a Downshift.** If any task in a feature is rerun because of a conflict, the remaining tasks in that feature run sequentially.
- **FR-S14 Conflicts between tasks.** When a task's winning branch conflicts with previously landed work, the task is rerun from the updated integration branch. A dedicated conflict-resolver agent is added only if measured rerun costs justify it.
- **FR-S15 Partial progress.** When one task needs attention, the owner is notified immediately and unaffected tasks continue. The feature request moves to `needs_attention` only when no task is running and at least one task is stuck.
- **FR-S16 Branch model.** Only two branch kinds are pushed: `agent/<issue>/integration`, which accumulates one commit per completed task and is the durable record of in-progress work, and `agent/<issue>/pr-<n>`, cut from it. Attempt and task branches stay in the worker workspace; losing attempts are kept as artifacts. Force-pushes are allowed only to `agent/` branches and only with `--force-with-lease`.
- **FR-S17 Commit conventions.** Commits are authored by the GitHub App's bot identity. Each task becomes one squashed commit. Messages follow the repository's convention, defaulting to Conventional Commits, with trailers recording task, plan version, model, and platform. Repositories requiring signed commits need the API commit path (config flag, deferred).
- **FR-S18 Stacked PRs.** A single PR is the default; a stack is used only when the total diff exceeds the repository's size cap, grouping small adjacent tasks, with a maximum depth of 4. Each PR branch is the integration branch truncated at a task commit. The final PR uses `Closes`; others use `Part of`. PRs open as drafts and become ready when the repository's CI passes.
- **FR-S18a Restacking.** Each PR's base SHA is recorded at branch creation. When a PR merges, the platform rebases each PR above it with `git rebase --onto <new-base> <recorded-base-sha>` and force-pushes with lease. A restack conflict moves that PR to `needs_attention`.
- **FR-S19 Base commit pinning.** The base SHA is pinned at plan approval and all tasks run against it. Exactly one rebase onto the latest default branch happens at finalization, followed by a full gate rerun. A conflict there reruns the affected task from the updated base.
- **FR-S20 Branch cleanup.** PR branches are deleted on merge, integration branches after all their PRs close, and a reaper removes `agent/` branches untouched for 30 days.

### Accuracy: results are correct

- **FR-A1 Deterministic gates first.** Build, lint and typecheck, existing tests, new tests, secret scan, and dependency scan all run before any LLM judging.
- **FR-A2 Hidden tests.** A portion of the test writer's tests is withheld from implementers and applied only during gating.
- **FR-A3 Gate-weakening detection.** Attempts that delete or skip tests, alter test configuration, or touch protected paths are rejected unless the owner explicitly approves.
- **FR-A4 Flaky tests.** A failing test is rerun against the unmodified base commit before an attempt is blamed for it.
- **FR-A5 Freshness.** Before a PR opens, the work is rebased onto the latest default branch and all gates rerun.
- **FR-A6 Grounded explanations.** Rationale is recorded at the moment each decision is made. `/ask` answers cite recorded decisions and specific files rather than reconstructing reasons after the fact.
- **FR-A7 Real CI.** A PR is considered ready only when the repository's actual CI passes.

### Interoperability: the system works with other systems

- **FR-I1 GitHub.** Integration uses a GitHub App (webhooks and REST API). No personal access tokens.
- **FR-I2 Model providers.** Anthropic, OpenAI, Google, and Ollama are accessed through a role-based router. Adding a provider means writing one adapter; no other code changes.
- **FR-I3 Sandbox backends.** Docker and Daytona implement the same sandbox interface.
- **FR-I4 Toolchains.** Build, test, and lint commands come from per-repository configuration. No language-specific logic is hardcoded in the platform.
- **FR-I5 Standard protocols.** LSP for code intelligence, OpenTelemetry for tracing, and the S3-compatible API for artifact storage.
- **FR-I6 Repository conventions.** The platform follows each repository's commit message style, CODEOWNERS file, and PR template when present.

### Security: the system resists misuse

- **FR-SEC1 Command authorization.** Only the owner's GitHub account, identified by its numeric user ID rather than its username, can trigger runs or issue commands. Commands from anyone else are ignored and logged.
- **FR-SEC2 Webhook verification.** Every webhook signature is verified; unsigned or invalid requests are rejected.
- **FR-SEC3 Least privilege.** Each agent role has an explicit tool list. Planner, judge, and discussion roles are read-only.
- **FR-SEC4 Sandbox isolation.** Sandboxes hold no credentials, deny network access by default (with a package-registry allowlist during dependency installation), enforce CPU, memory, disk, and time limits, never mount the Docker socket, and are destroyed after use.
- **FR-SEC5 Platform-only external actions.** Only platform code, never an agent, pushes branches or opens PRs, and only after gates pass.
- **FR-SEC6 Untrusted content.** Issue text, repository files, and dependency documentation are treated as data and are never merged into system instructions.
- **FR-SEC7 Secrets.** Secrets never appear in logs, traces, artifacts, or prompts, and are loaded from environment files outside the repository.
- **FR-SEC8 Protected paths.** Per-repository protected paths (CI configuration, authentication, infrastructure, deployment) require explicit owner approval for any change.
- **FR-SEC9 Budget enforcement.** Caps apply per attempt, per feature request, and per month. Hitting a cap pauses work rather than discarding it.
- **FR-SEC10 Audit log.** Every command, state transition, and external action is recorded with a timestamp and actor.
- **FR-SEC11 Kill switch.** A single command cancels all in-flight runs and stops workers.
- **FR-SEC12 Sensitivity tiers.** Every repository has a tier (`public`, `private`, `sensitive`, or `local_only`) set in the platform repository's configuration. A repository without a tier cannot start a run.
- **FR-SEC13 Tier enforcement.** The tier is enforced at the LLM gateway for every model request, including fallbacks and evaluation candidates, and also governs embedding models, trace export, artifact storage, and published reports. See `security-and-threat-model.md`.
- **FR-SEC14 Dependency additions.** Sandboxes have network access only during a platform-controlled install phase. Agents request new dependencies through a platform tool that checks the registry allowlist, known vulnerabilities, license, and typosquatting signals, and flags additions for owner approval.
- **FR-SEC15 GitHub App permissions.** The App holds only the permissions it needs (contents, pull requests, issues) and never holds `workflows` or administration permissions.
- **FR-SEC16 Branch rules.** The App pushes only to `agent/` branches. Default branches are protected with no bypass for the App.
- **FR-SEC17 Pre-run scans.** Sandboxes use shallow clones. Before any repository content is sent to a model, a secret scan runs; for `sensitive` and `local_only` tiers, a personal-data scan also runs. Findings block the run.
- **FR-SEC18 Network exposure.** Only the webhook endpoint is publicly reachable. Internal services are reachable only over Tailscale, restricted by access rules.
- **FR-SEC19 Risk surfacing.** Plan comments begin with a risk summary. Plans touching protected paths, adding dependencies, or changing authentication or network code require `/approve --accept-risk`. PR descriptions include a security-relevant changes section.
- **FR-SEC20 Canary detection.** Canary secrets are planted in test fixtures and environments. Any canary appearing in a prompt, log, trace, artifact, comment, or PR halts the run.
- **FR-SEC21 Sandbox git credentials.** Platform code clones into the worker's trusted workspace and copies the working tree into the sandbox with the remote and any credentials removed from `.git/config`. Sandboxes never hold GitHub credentials.
- **FR-SEC22 Sensitive path substitution.** Per-repository excluded paths are listed in the platform repository's configuration, never in the target repository. Before a working tree enters a sandbox, each excluded file is replaced with a synthetic file of the same name and schema, or omitted where nothing depends on it. Excluded paths are also skipped when building embeddings and codebase briefs. Enforcement is by physical absence, not by tool policy, because agents can read any present file through shell commands. Onboarding verifies the repository builds and its tests pass with substitutions applied.
- **FR-SEC23 Sandbox environment.** Sandboxes receive an allowlisted environment only: `PATH`, `HOME`, `LANG`, toolchain variables, and `CI=true`. Repository-specific variables come from a `sandbox_env` block in the platform repository's configuration holding fake values, with canary values wherever an external service would be called. The worker's own environment is never inherited. Repositories that load `.env` files get a committed `.env.example` copied into place during the install phase; `.env` is an excluded path and is covered by the pre-run secret scan.
- **FR-SEC24 Tool call logging.** Every agent tool call is recorded by the worker, never from inside the sandbox, as a queryable row holding attempt, step, tool, full arguments, exit code, duration, and a pointer to full output in the blob store. Retention follows the repository's tier. Tool calls are scanned during gating for suspicious patterns (network attempts, writes outside the workspace, encoded-then-executed payloads, reads near excluded paths); matches flag the attempt.
- **FR-SEC25 Execution surface changes.** The gate flags any diff touching files that cause code execution at install, build, or commit time: package manager lifecycle scripts, hook installers (`.husky/`, `lefthook.yml`, `.pre-commit-config.yaml`), registry configuration (`.npmrc`, `.yarnrc`), `.gitattributes` filters, Makefiles, Dockerfiles, devcontainer and editor task configuration, and build plugin declarations. The list also covers AI tooling configuration that takes effect when the owner next opens the repository: `.mcp.json`, `.claude/`, `.cursor/`, `CLAUDE.md`, and `AGENTS.md`. Flagged attempts are labeled and called out at the top of the PR description. The list has language defaults plus per-repository additions.
- **FR-SEC26 Self-modification.** The platform's own repository has a hardcoded protected-path list covering security, gate, configuration, and policy code that no agent may edit. Changes to the platform repository require human review with no risk-acceptance shortcut. The platform does not run on its own repository before Milestone 2 is complete.
- **FR-SEC27 Gate-to-push integrity.** When the agent phase ends, all sandbox processes are killed, the diff is extracted and hashed, and gates run against that recorded artifact. The gate result records the artifact hash. The push path requires a passing gate result and refuses any artifact whose recomputed hash does not match. Any transformation, including the finalization rebase, produces a new artifact that must be re-gated and re-hashed. The gating mode is configurable per repository: `clean_room` rebuilds a fresh sandbox from the pinned base commit plus the diff alone, so no agent-modified environment carries over; `reset_in_place` resets the agent's sandbox to base and re-applies the recorded diff, preserving the artifact guarantee but not the environment guarantee. The default is `reset_in_place`, with `clean_room` for `sensitive` repositories and for any attempt flagged by the tool call scan or the execution surface check. Milestone 2 measures the cost difference and sets the default from data. Gating is never skipped; if the gating environment cannot be built, the attempt is marked `errored` rather than `failed_gates`, retried once, and escalated on repeat.
- **FR-SEC28 Content provenance.** Every piece of text entering agent context carries a provenance label, and authorization is independent of context. Only the owner's numeric user ID can issue commands (FR-SEC1); the owner's text is the request but cannot grant an agent a capability its role's tool list does not include. Agents read canonical records from the database (plan, decisions, gate results) rather than re-reading the platform's own rendered comments, which prevents generated output from re-entering as input while keeping the plan available to the discussion role. Comments from other humans and from bots are quarantined as untrusted data; bot comments are treated as relaying third-party text, since dependency bots and CI bots embed content authored elsewhere. On public repositories, non-owner comments are excluded from agent context by default. Generated markdown is sanitized before posting, removing mass mentions and hidden HTML. Provenance is determined from the webhook payload's author identity (numeric user ID, user type, and App flag), never from markers in the comment body, which are forgeable. Quarantined text is wrapped in delimited blocks carrying provenance attributes and a random per-request nonce in the delimiter; the nonce is stripped from the content before wrapping, so quarantined text cannot close its own block and escape.
- **FR-SEC29 Path validation.** File tools operate through a workspace root handle (`os.Root`) so containment is enforced at the syscall level, covering symlinks and the check-then-write race. Where a root handle is unavailable, the parent directory is resolved through symlinks before joining the filename, and containment is checked with a separator-aware relative-path comparison rather than a string prefix. Absolute paths, `..` segments, and encoded traversal sequences are rejected, never sanitized. Containment is combined with a denylist of sensitive in-workspace paths, including `.git` internals and substituted fixture files. Every rejection is logged, and repeated rejections within one attempt flag the attempt. Path validation constrains the file tools and keeps the diff clean; the sandbox, not path validation, is the boundary against shell commands.
- **FR-SEC30 Images and backups.** Sandbox base images are built by CI in the platform repository and published to GitHub Container Registry as private packages; workers pull by digest, never by tag. A weekly scheduled job rebuilds and scans them, and digest updates arrive as reviewed PRs to the platform repository. Images serving a `local_only` repository are built locally and never pushed. Postgres is dumped nightly to the central machine's disk and copied offsite to Cloudflare R2 through restic, which encrypts client-side so the storage provider holds ciphertext only; retention is 7 daily, 4 weekly, 6 monthly. Secrets are never included in backups and live in a password manager. The restic repository key is stored somewhere other than the machine being backed up. Restores are tested monthly against a scratch database. Temporal's own database is not backed up; losing it loses in-flight workflow state only. Machines running workers use full disk encryption.
- **FR-SEC31 Alerting.** Alerts are opened as issues in the platform's repository, labeled and assigned to the owner, so existing GitHub notifications deliver them. Three severities: `halt` (canary hit, kill switch, repeated authorization failure from an unknown actor) stops all work; `pause` (budget cap) stops one feature pending `/resume`; `digest` batches everything else weekly. Conditions already visible elsewhere, such as flagged attempts shown on their PR, do not also generate alerts. Each alert states what happened, where, what automatic action was already taken, and what the owner must do, with a link to the relevant runbook. Evidence is given as identifiers and local artifact paths only; canary values, secret material, and repository content are never included, and tier rules apply to alert content. A dedupe key updates an existing open issue rather than opening duplicates. Every alert is written to a local append-only file on the central machine before delivery is attempted, and delivery failures are recorded there, so the record survives a compromised or unavailable GitHub path.
- **FR-SEC32 Access matrix.** Each repository's configuration lists the agent roles permitted to run on it. Roles not listed are denied, so adding a new role grants it nothing by default. The platform repository's protections come from a hardcoded table that configuration cannot widen; before Milestone 2 only the read-only `discussion` role is permitted there. A repository missing either a tier or a role allowlist refuses to start. A human-readable access matrix is generated from the configuration by CI rather than maintained by hand, and each plan comment reports the roles that ran.

### Compliance: the system follows rules and policies

- **FR-C1 Licenses.** New dependencies are checked against a per-repository license allowlist.
- **FR-C2 Provider restrictions.** Allowed model providers are determined by each repository's sensitivity tier (FR-SEC12, FR-SEC13).
- **FR-C3 AI attribution.** Commits and PRs produced by the platform are labeled as AI-generated.
- **FR-C4 Test data only.** Sandboxes use seed or fixture data only.

## Non-Functional Requirements

### Scalability

- **NFR-SC1 Horizontal workers.** Capacity is added by starting a worker binary on another machine, with no central configuration change.
- **NFR-SC2 Initial capacity target.** 3 concurrent feature requests, 3 attempts per task, and about 10 concurrent sandboxes across machines. A global sandbox limit applies regardless of task concurrency and attempt settings.
- **NFR-SC3 Central limits.** Per-repository concurrency limits and model rate limits are enforced centrally, not per worker.
- **NFR-SC4 Growth path.** Documented upgrade paths exist for remote sandboxes (Daytona), managed orchestration (Temporal Cloud), and remote artifact storage (Cloudflare R2).

### Performance

Initial targets, to be revised using Milestone 2 measurements.

- **NFR-P1 Webhooks.** Webhooks are acknowledged within 2 seconds, well within GitHub's 10-second delivery timeout; all work happens asynchronously.
- **NFR-P2 Feedback.** A reaction or status comment appears within 30 seconds of any label or command.
- **NFR-P3 Discussion.** Typical `/ask` responses arrive within 2 minutes.
- **NFR-P4 Sandbox startup.** Sandboxes are ready within 60 seconds using prebuilt images.
- **NFR-P5 Brief reuse.** Codebase briefs are reused for the same commit and updated incrementally on new commits.
- **NFR-P6 Cost tracking.** Cost per resolved task is tracked; no fixed target until a Milestone 2 baseline exists.

### Maintainability (human perspective)

**Guiding principle:** a developer who knows Go should understand any package after about an hour of reading, without help from the original author. The owner must be able to explain every package.

Platform code:

- **NFR-M1 Boring code.** Standard library first. No web frameworks, no dependency-injection containers, no reflection-based magic. Code generation is limited to sqlc. Generics only when they remove clear duplication.
- **NFR-M2 Simple control flow.** Early returns, nesting no deeper than three levels, functions around 60 lines or fewer as a guideline, and every error wrapped with context.
- **NFR-M3 Documented packages.** Each package has a single responsibility and a `doc.go` explaining its purpose and how it fits into the system.
- **NFR-M4 Justified dependencies.** Every new third-party dependency requires a decision record.
- **NFR-M5 Interfaces at real boundaries only.** Sandbox, model provider, blob store, and GitHub client. Not for every struct.
- **NFR-M6 Readable tests.** Table-driven tests, fakes preferred over mocks, one behavior per test case.
- **NFR-M7 Plain configuration.** YAML files validated at startup with clear error messages.
- **NFR-M8 Enforced limits.** Complexity limits are enforced in CI through golangci-lint (`gocyclo`, `gocognit`, `funlen`, `nestif`).
- **NFR-M9 Decision records.** Every non-obvious choice is recorded in `docs/decisions/`; in-depth topics are tracked in `docs/review-list.md`.

Generated code in target repositories:

- **NFR-M10 Simplest sufficient solution.** Generated code follows the target repository's existing conventions and uses the simplest implementation that satisfies the acceptance criteria.
- **NFR-M11 Complexity penalized.** The judge rubric penalizes unnecessary abstraction, unrequested refactoring, and oversized diffs.

### Reliability

- **NFR-R1 No lost work.** A worker crash never loses progress; work resumes on another worker.
- **NFR-R2 Idempotent actions.** Every external action is safe to retry without duplicating effects.
- **NFR-R3 Guaranteed cleanup.** Sandboxes are cleaned up even after cancellation or failure, and a periodic reaper removes any orphans.
- **NFR-R4 Backups.** Postgres is backed up daily.

### Usability

- **NFR-U1 GitHub-native.** All interaction happens through GitHub; `/help` lists available commands.
- **NFR-U2 Readable status.** A single status comment is updated in place and is understandable without opening any other tool.
- **NFR-U2a Comment format.** Platform comments open with a header line identifying the feature, plan version, and run settings, place long content in collapsible sections, and close with the available commands. Commands mentioned in prose are wrapped in backticks so platform text never reads as a command. GitHub's own bot badge and `[bot]` suffix carry the author distinction; no text marker is used for it.
- **NFR-U3 Actionable errors.** Every error or escalation message states what happened and which options are available.

### Portability

- **NFR-PO1 Platforms.** Runs on macOS and Linux. Workers are single binaries; central services run through Docker Compose.
