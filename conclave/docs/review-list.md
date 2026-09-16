# In-Depth Review List

Topics the owner must understand in depth: well enough to explain them without notes, review AI-generated code for them, and defend them in an interview. Item IDs are permanent; tickets reference them (for example, "touches RL-13").

## How to Use This List

- **Before a ticket:** read the items the ticket references.
- **Self-check:** each item has questions without answers. Mark an item understood only when you can answer every question without looking anything up. If you can't, that's the signal to study it before reviewing the code.
- **When reviewing a PR:** use the questions as a checklist against the actual code.
- **Keep it current:** when the design changes, update the affected item and note it in the change log.

## Learning Order by Milestone

| Milestone | Items, in suggested order |
|---|---|
| 1 | RL-18, RL-1, RL-14, RL-13, RL-9, RL-2, RL-3, RL-16, RL-5, RL-11, RL-6, RL-7, RL-8, RL-17, RL-4 |
| 2 | RL-4 (embedding sections), RL-20, RL-22 |
| 3 | RL-6 (provider adapter sections), RL-19 |
| 4 | RL-15, RL-21, RL-10 |
| 5 | RL-16 (recorded decision sections) |

---

## Workflows and Orchestration

### RL-1 Temporal Fundamentals
- [ ] Understood

**What it covers:** workflows versus activities, determinism, replay, retry policies, heartbeats, and signals.
**Why it matters:** Temporal is the backbone. Misunderstanding it produces bugs that only appear after crashes or retries.
**Needed by:** Milestone 1 · **Related:** NFR-R1

**Self-check**
1. A worker crashes halfway through a workflow. Walk through exactly how another worker continues without repeating completed steps.
2. Why can a workflow call a model only through an activity?
3. What is the difference between an activity's start-to-close timeout and its heartbeat timeout, and what kind of problem does each catch?
4. How does a signal resume a workflow that has been waiting days for `/approve`?

### RL-2 Idempotent Activities
- [ ] Understood

**What it covers:** designing every activity so a retry cannot open a duplicate PR, push twice, or double-count cost.
**Why it matters:** Temporal retries activities automatically, and a retry can happen after the side effect already succeeded.
**Needed by:** Milestone 1 · **Related:** NFR-R2

**Self-check**
1. The "open PR" activity succeeds on GitHub, but the worker dies before reporting success. What happens on retry, and what in your design prevents a second PR?
2. Which Milestone 1 activities are naturally safe to repeat, and which need explicit protection?
3. How does a cost event avoid being recorded twice?

### RL-3 Guaranteed Sandbox Cleanup
- [ ] Understood

**What it covers:** deferred cleanup using disconnected contexts, plus the orphan reaper.
**Why it matters:** a cleanup bug silently fills disks and leaves containers running.
**Needed by:** Milestone 1 · **Related:** NFR-R3

**Self-check**
1. Why does a cleanup activity started with the workflow's normal context never run after the workflow is cancelled?
2. If the cleanup activity itself fails, what ensures the container is eventually removed?
3. Which database field does the reaper rely on, and why is cleanup tracked there instead of as an attempt status?

### RL-9 Feature Request Lifecycle
- [ ] Understood

**What it covers:** the six statuses (`planning`, `awaiting_approval`, `executing`, `needs_attention`, `delivered`, `cancelled`), the `reason` field on `needs_attention`, every legal transition, how cancellation interacts with each status, and how task and attempt statuses grow by milestone.
**Why it matters:** undefined transitions are where confusing bugs come from.
**Needed by:** Milestone 1 · **Related:** FR-S5, FR-S11, FR-S12

**Self-check**
1. For each status, which commands are valid, and what happens when an invalid one arrives?
2. A `/cancel` arrives while `executing` is in the middle of pushing a branch. What happens, step by step?
3. Why is there no automatic `failed` status?
4. Why does the workflow end at `delivered` instead of waiting for the PR to be merged?

### RL-10 Continue-As-New
- [ ] Understood

**What it covers:** keeping the long-running merge queue workflow's history bounded.
**Why it matters:** workflow histories have size limits, and a merge queue runs indefinitely.
**Needed by:** Milestone 4

**Self-check**
1. Why does the merge queue workflow need Continue-As-New when a feature workflow doesn't?
2. What state must be carried into the new run?
3. How do you avoid losing a signal that arrives just before the new run starts?

### RL-13 Compare-and-Set State Transitions
- [ ] Understood

**What it covers:** conditional `UPDATE ... WHERE state = expected` statements plus a single transition table in Go.
**Why it matters:** this is how statuses stay legal despite retries, duplicate webhooks, and races such as a cancellation arriving during an approval.
**Needed by:** Milestone 1

**Self-check**
1. An update that matches zero rows can mean two different things. What are they, and how does the code tell them apart?
2. How does this pattern handle a duplicate webhook? A retried activity?
3. Where does the transition table live, and what does its test check?

### RL-14 Temporal Versus Postgres Responsibilities
- [ ] Understood

**What it covers:** Temporal owns execution state; Postgres owns domain records and is the source of truth for anything queried or reported.
**Why it matters:** putting durable data only in workflow history means losing it.
**Needed by:** Milestone 1

**Self-check**
1. To answer "which attempts failed gates last week," which system do you query, and why not the other?
2. What happens to a closed workflow's history after the retention period?
3. How does a workflow change a record in Postgres without breaking determinism?

### RL-15 Parent-Driven Task Scheduling
- [ ] Understood

**What it covers:** the selector-based scheduler loop, recomputing ready tasks after each completion, footprint-based serialization, and when a feature request moves to `needs_attention`.
**Why it matters:** sequential and parallel execution run the same scheduler code.
**Needed by:** Milestone 4 · **Related:** FR-S13, FR-S13a, FR-S15

**Self-check**
1. After a child workflow completes, how does the parent decide what to start next?
2. T2 moves to `needs_attention` while T3 is running. What happens to T3, T4, T5, and the feature request's status?
3. How do predicted file footprints change the task graph before scheduling begins?

### RL-16 Determinism in Workflow Logic
- [ ] Understood

**What it covers:** why Go map iteration order breaks replay and ready tasks must be sorted; why decisions like the parallelism limit are computed in activities and recorded rather than calculated in workflow code.
**Why it matters:** determinism bugs pass every normal test and fail only during replay.
**Needed by:** Milestone 1 (general rules), Milestone 5 (recorded decisions)

**Self-check**
1. Why can ranging over a Go map inside workflow code break replay?
2. Name three other common determinism violations and the Temporal-safe alternative for each.
3. If the parallelism formula were calculated inside the workflow and you later changed a threshold, what would happen to workflows already in flight?

---

## Data

### RL-4 Postgres Schema Design
- [ ] Understood

**What it covers:** how tasks, dependencies, attempts, and evaluations relate; the constraints that keep them consistent; pgvector's index dimension limit; choosing embedding dimensions; storing the embedding model with each vector.
**Why it matters:** the schema encodes the invariants the whole system depends on.
**Needed by:** Milestone 1 (core schema), Milestone 2 (embeddings)

**Self-check**
1. Which constraint guarantees only one approved plan per feature request, and why is it a partial unique index rather than a plain one?
2. A task is marked integrated, but its selected attempt failed a gate. Which rule should have prevented that, and where is it enforced?
3. Why can't a standard pgvector approximate-nearest-neighbor index handle 3,072-dimension vectors, and what are your options?
4. Why store the embedding model's name alongside each vector? What goes wrong if you don't and later switch models?

---

## Execution and Agents

### RL-5 Sandbox Isolation
- [ ] Understood

**What it covers:** resource limits, network restrictions, the install and agent phases, hardened container settings (non-root user, dropped capabilities, `no-new-privileges`, process limits, read-only root filesystem), gVisor, and never mounting the Docker socket.
**Why it matters:** the sandbox is the wall between untrusted code and your machines.
**Needed by:** Milestone 1 (gVisor in Milestone 4) · **Related:** FR-SEC4

**Self-check**
1. List every way sandboxed code could reach the network or the host, and the setting that blocks each.
2. Why is mounting `/var/run/docker.sock` into a container equivalent to giving it root access to the host?
3. What happens in the install phase versus the agent phase, and why is the network cut before the agent gets control?

### RL-6 The Agent Loop and Provider Adapters
- [ ] Understood

**What it covers:** tool execution, feeding results back, step limits, token budgets, and how provider adapters normalize tool calls into one format.
**Why it matters:** adapter bugs don't look like adapter bugs; they look like agents behaving strangely.
**Needed by:** Milestone 1 (loop), Milestone 3 (adapters) · **Related:** FR-I2

**Self-check**
1. Trace one full iteration: the model requests a tool, the tool runs in the sandbox, the result returns. Where are step limits and token budgets checked?
2. The model requests two tool calls in one turn, and one fails. What does the next request to the model contain?
3. An agent keeps calling the same tool with the same arguments. How would you tell whether that's model behavior or an adapter bug?

### RL-11 Least-Privilege Agent Permissions
- [ ] Understood

**What it covers:** per-role tool allowlists as the primary defense against prompt injection.
**Why it matters:** a manipulated agent can only misuse the tools it has.
**Needed by:** Milestone 1 · **Related:** FR-SEC3

**Self-check**
1. List each role's tools. Which roles can write files, and where can they write?
2. If the planner is fully hijacked by an injected instruction, what is the worst thing it can do?
3. Why are pushing branches and opening PRs done by platform code instead of an agent tool?

---

### RL-21 Stacked PR Restacking
- [ ] Understood

**What it covers:** recording each PR's base SHA, rebasing with `--onto` after a lower PR merges, force-pushing with lease, and handling restack conflicts.
**Why it matters:** a plain rebase after a squash merge replays already-merged work and produces conflicts or duplicated diffs.
**Needed by:** Milestone 4 · **Related:** FR-S18, FR-S18a

**Self-check**
1. You squash-merge `pr-1`. Why does `git rebase main` on `pr-2` misbehave, and how does `--onto` with the recorded base SHA avoid it?
2. What does `--force-with-lease` refuse to do that `--force` would allow, and why does that matter here?
3. A stack is three deep and the middle PR's restack conflicts. What happens to it and to the PR above it?

---

### RL-22 Platform Self-Modification
- [ ] Understood

**What it covers:** why the platform's own repository is the highest-risk target, its hardcoded protected paths, mandatory review, and why self-runs wait until Milestone 2.
**Why it matters:** a change to gate, tier, or policy code governs every future run on every repository, not just the PR it appears in.
**Needed by:** Milestone 2 · **Related:** FR-SEC26

**Self-check**
1. Name four changes to the platform repository that would silently weaken security on unrelated repositories.
2. Why is the protected-path list for the platform repository hardcoded rather than read from configuration?
3. Why does self-modification get no risk-acceptance shortcut when other risky plans do?
4. Why is the role list an allowlist rather than a denylist, and what breaks if you invert it?

---

## Security
### RL-7 GitHub App Authentication and Permissions
- [ ] Understood

**What it covers:** installation tokens, permission scoping, webhook signature verification, authorizing commands by numeric user ID, withholding the `workflows` permission, and branch rules without App bypass.
**Why it matters:** GitHub access is the platform's most powerful capability.
**Needed by:** Milestone 1 · **Related:** FR-I1, FR-SEC1, FR-SEC2, FR-SEC15, FR-SEC16

**Self-check**
1. How does the App turn its private key into an installation token, and when does that token expire?
2. How is a webhook signature computed and verified, and why must the comparison be constant-time?
3. Why authorize commands by numeric user ID rather than username?
4. What exactly stops the platform from modifying `.github/workflows`, even if an agent writes such a change?

### RL-8 Secrets Handling
- [ ] Understood

**What it covers:** keeping keys out of sandboxes, prompts, logs, traces, artifacts, and the repository; scrubbing git remotes and credentials from `.git/config` before a working tree enters a sandbox; development-time controls (secrets outside the project directory, deny rules plus a `PreToolUse` hook, the platform's API key kept out of the shell environment).
**Why it matters:** a leaked key undoes every other control.
**Needed by:** Milestone 1 · **Related:** FR-SEC7

**Self-check**
1. Name every place a secret could leak in this system and the control for each.
2. Why could exporting the platform's `ANTHROPIC_API_KEY` in your shell profile change how Claude Code bills you?
3. Why aren't Claude Code deny rules enough on their own?

### RL-17 Sensitivity Tier Enforcement
- [ ] Understood

**What it covers:** the gateway as the single choke point, no-tier-no-run, covering fallbacks, candidates, embeddings, traces, artifacts, and published reports; and excluding sensitive files by substitution before sandbox entry rather than by tool policy.
**Why it matters:** one missed path, like an embedding call that bypasses the gateway, defeats the whole scheme.
**Needed by:** Milestone 1 · **Related:** FR-SEC12, FR-SEC13, FR-SEC22, FR-C2

**Self-check**
1. List every path through which code from a `sensitive` repository could leave your machines, and where each is checked.
2. The planner's primary model is unavailable, and its fallback isn't allowed for this repository's tier. What happens?
3. Why does an unclassified repository refuse to run instead of defaulting to a tier?
4. Why can't excluded files be protected by denying the read tool, and what is done instead?

### RL-18 Blast-Radius Design
- [ ] Understood

**What it covers:** assuming any agent can be manipulated, and limiting what a manipulated agent can reach.
**Why it matters:** it's the reasoning behind nearly every other security control.
**Needed by:** Milestone 1 · **Related:** security principles in `security-and-threat-model.md`

**Self-check**
1. Assume the implementer in a sandbox is fully controlled by an attacker. What can it reach, and what stops it from reaching more?
2. What is the worst realistic outcome of a hijacked agent, and which controls keep it no worse than that?
3. Why doesn't the design rely on detecting prompt injection?

### RL-19 Dependency and Execution-Surface Changes
- [ ] Understood

**What it covers:** why agents request dependencies through a platform tool instead of installing them, what the tool checks, and which files cause code to execute at install, build, or commit time.
**Why it matters:** dependencies and build-time hooks run code outside the application's own runtime, where normal review attention rarely lands.
**Needed by:** Milestone 3 · **Related:** FR-SEC14, FR-SEC25, FR-C1

**Self-check**
1. Why can't an agent run `npm install` during the agent phase?
2. What does the dependency tool check before flagging an addition for approval?
3. What is typosquatting, and what signals suggest a package might be a typosquat?
4. Name four tracked files that can cause code to run without anyone launching the application, and when each fires.
5. Which files in a diff could change how your own Claude Code sessions behave, and when would that take effect?
6. Why do sandbox commands matter less than the diff, and what does that imply about where checks belong?

### RL-20 Canary Tokens and Security Tests
- [ ] Understood

**What it covers:** planted fake secrets, the canary detector, prompt injection fixtures, and sandbox attack tests.
**Why it matters:** they prove security controls hold in AI-generated code instead of assuming they do.
**Needed by:** Milestone 2 · **Related:** FR-SEC20

**Self-check**
1. What is a canary token, and what does it prove that a passing unit test doesn't?
2. Where are canaries planted, and which outputs does the detector scan?
3. Describe one sandbox attack test and what a failure would indicate.

---

## Working Understanding

Know how these work and be able to review their code, without mastering every detail.

- **LLM router:** role-to-model mapping, fallbacks, budgets, and rate limits
- **Outside-provider judge rule:** which provider judges each pair, and the restricted-tier fallback
- **Deterministic evaluation gate:** which checks run, in what order, and how gate-weakening is detected
- **Language server integration:** how LSP clients answer definition and reference queries
- **Merge queue basics:** rebase, rerun gates, land
- **Workflow versioning (formerly RL-12):** the drain procedure for upgrades; becomes in-depth again only if long waits return to workflows
- **Footprint-prediction accuracy:** how predicted and actual changed files are compared
- **Claude Code configuration:** subagents, hooks, permission rules, and custom commands for the ticket loop

## Black Boxes

Know what each does; let the tools and agents handle the specifics.

- sqlc and goose
- golangci-lint and other linters
- OpenTelemetry setup
- Tailscale setup
- Docker Compose files
- GitHub Actions configuration

## Change Log

- **RL-4:** expanded with pgvector dimension limits and embedding model tracking
- **RL-5:** expanded with hardened container settings and install and agent phases
- **RL-6:** expanded with provider adapter normalization
- **RL-7:** expanded with numeric user IDs, `workflows` permission, and branch rules
- **RL-8:** expanded with development-time secrets controls and git credential scrubbing before sandbox entry
- **RL-9:** revised to the six-status lifecycle
- **RL-12:** downgraded to working understanding after workflows were changed to end at delivery
- **RL-15:** restored to in-depth after sequential and parallel execution were unified in one scheduler
- **RL-16:** expanded from map iteration to recorded decisions
- **RL-17 through RL-20:** added with the security plan
- **RL-19:** expanded from dependency additions to include execution-surface changes
- **RL-22:** added for platform self-modification
- **RL-21:** added with the git and PR conventions
