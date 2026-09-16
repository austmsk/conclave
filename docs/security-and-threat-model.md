# Security and Threat Model (Draft v0.1)

Requirement IDs refer to `scope-and-requirements.md`.

## Security Principles

1. **Assume any agent can be manipulated.** Prompt injection has no complete defense. Every agent is treated as potentially hostile, and security comes from limiting what a manipulated agent can reach. The worst realistic outcome of a fully hijacked agent must be a rejected PR and wasted spend.
2. **Deny by default.** Unclassified repositories do not run. Providers, network destinations, tools, and permissions are allowlists.
3. **Enforce at choke points.** Each policy is enforced in exactly one place that all traffic passes through: the LLM gateway for model egress, the GitHub client for repository writes, and the sandbox manager for code execution.
4. **Agents propose; platform code acts.** Every external action (pushing, opening PRs, commenting, installing dependencies) is performed by deterministic platform code after checks.
5. **Humans approve risk explicitly.** Risk is surfaced at the top of plans and PRs, never buried.
6. **Test controls by attempting the attack.** Every control has an automated test that tries to defeat it.

## Sensitivity Tiers

Every repository has a tier, set in the platform repository's configuration. A target repository cannot change its own tier. A repository without a tier cannot start a run.

| Tier | Model providers | Traces | Artifacts | Published reports | Extra controls |
|---|---|---|---|---|---|
| `public` | Any allowlisted provider, including hosted open-weight candidates | Any | Any | Allowed | None |
| `private` | Anthropic, OpenAI, Google (paid API tiers only) | Self-hosted only | Local disk or Cloudflare R2 | Aggregates only | Shallow clones |
| `sensitive` | Explicit per-repository list approved by the owner after reviewing provider data terms | Local only | Local only | Aggregates only | Shallow clones; pre-run secret and personal-data scan |
| `local_only` | Local models only | Local only | Local only | Aggregates only | All `sensitive` controls |

### Suggested Assignments (owner to confirm)

| Repository | Suggested tier |
|---|---|
| Election Tally System, C++ Multicore Simulator, this platform (if public) | `public` |
| PEI, PeHF, and GPPS websites | `private` |
| Premier Health website; clinic and pharmacy reporting tools | `sensitive` |

### Tier Rules

- **Single enforcement point.** The LLM gateway checks the tier on every request using the request's repository ID. The check applies to primary models, fallbacks, and evaluation candidates.
- **All egress covered.** Tier policy also governs embedding models, trace exporters, artifact storage, and published evaluation reports.
- **Restricted-tier degradation.** When a tier allows fewer providers than the attempt pool needs, attempts use different models or settings from allowed providers. When the outside-provider judge rule cannot apply, the judge comes from an allowed provider with randomized presentation order, and the comparison is flagged `same_provider_judge`.
- **Audited changes.** Tier changes happen through reviewed commits to the platform repository and are recorded in the audit log.
- **Provider terms.** Before adding a provider to the `private` or `sensitive` lists, verify its current data-retention and training terms, including differences between free and paid tiers.

## Assets

| Asset | Why it matters |
|---|---|
| Source code of target repositories | Confidentiality varies by tier |
| Credentials: GitHub App private key, installation tokens, model API keys, database and storage credentials, Tailscale keys | Grant access to everything else |
| Integrity of target repositories (default branches, CI pipelines) | Malicious code could reach production sites |
| Owner's machines | Host compromise exposes all of the above |
| API budget | Direct financial loss |
| Evaluation data and audit log | Portfolio evidence and incident investigation |
| Personal data that may exist in sensitive repositories | Legal and ethical obligations to the people involved |

## Trust Boundaries

| ID | Boundary | What crosses it |
|---|---|---|
| B1 | Internet → webhook endpoint | GitHub webhook requests through Cloudflare Tunnel |
| B2 | GitHub content → agent context | Issue text, comments, repository files, dependency documentation |
| B3 | Worker → sandbox | Untrusted code execution |
| B4 | Platform → model providers | Code and prompts leaving owner control |
| B5 | Sandbox → network | Package registry access during the install phase only |
| B6 | Machine ↔ machine | Worker traffic to central services over Tailscale |
| B7 | Development environment ↔ platform secrets | Claude Code sessions on the platform repository |

## Threat Sources

- **Prompt injection** through any content an agent reads, including content in the owner's own repositories
- **Model misbehavior** without any attacker: deleting tests to pass gates, oversized changes, plausible but wrong code
- **Supply chain attacks**: malicious or typosquatted dependencies, compromised base images
- **External attackers**: forged webhooks, stolen tokens, exposed services
- **Owner account compromise**
- **Data exposure at third-party providers**
- **Human factors**: approving plans or merging PRs without adequate review

## Threats and Mitigations

### Spoofing

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| S1 | Forged webhook triggers a run | HMAC signature verification; unsigned requests rejected; optional restriction of the tunnel to GitHub's published webhook IP ranges | FR-SEC2 |
| S2 | Another GitHub user issues commands | Authorization by numeric GitHub user ID (usernames can change); other users' commands ignored and logged | FR-SEC1 |
| S3 | Owner's GitHub account is taken over | Two-factor authentication with a passkey or hardware key; kill switch; periodic audit log review | FR-SEC10, FR-SEC11 |

### Tampering

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| T1 | Agent weakens gates by deleting or skipping tests or editing CI | Gate-weakening detection; protected paths; repository configuration stored in the platform repository; GitHub App lacks the `workflows` permission, so CI files cannot be modified | FR-A3, FR-SEC8, FR-SEC15 |
| T2 | Agent reads or edits hidden tests | Hidden tests stored outside the sandbox and applied only during gating | FR-A2 |
| T3 | Changes land directly on a default branch | App pushes only to `agent/` branches; branch rules with no bypass for the App | FR-SEC5, FR-SEC16 |
| T4 | Malicious dependency is added | Dependency additions only through a platform tool: registry allowlist, vulnerability scan, license check, typosquatting and package-age heuristics, owner approval | FR-SEC14, FR-C1 |
| T4a | Diff installs a git hook or other build-time execution | Execution surface check flags lifecycle scripts, hook installers, registry configuration, `.gitattributes` filters, Makefiles, Dockerfiles, and build plugins; flagged in the PR and labeled | FR-SEC25 |
| T4b | Diff reconfigures the owner's AI tooling (`.mcp.json`, `.claude/`, `CLAUDE.md`, `AGENTS.md`) | Same execution surface check; these take effect when the owner next opens the repository, not in CI, so they are flagged explicitly. Applies with particular force to the platform's own repository | FR-SEC25 |
| T5 | Injected instruction persists in the codebase brief across runs | Brief treated as untrusted data; never placed in system instructions; rebuilt per commit | FR-SEC6 |
| T6 | Audit log or cost records are altered | Application database role has insert-only access to append-only tables | FR-SEC10 |
| T7 | Agent weakens the platform itself (gates, tiers, policy code) | Hardcoded protected paths in the platform repository; mandatory human review with no risk-acceptance shortcut; no self-runs before Milestone 2; per-repository role allowlist with a hardcoded layer configuration cannot widen | FR-SEC26, FR-SEC32 |
| T8 | Pushed artifact differs from the gated artifact | Processes killed at end of agent phase; diff frozen and hashed before gating; gates run against the recorded artifact in either gating mode; push path refuses any non-matching artifact | FR-SEC27 |
| T8a | Tests pass because of an agent-modified environment rather than the diff | `clean_room` gating for sensitive repositories and flagged attempts; nothing but the diff carries over. Accepted residual risk under `reset_in_place` | FR-SEC27 |
| T9 | Platform's own generated content re-enters as instructions or commands | Agents read canonical database records rather than rendered comments; App-authored comments never authorize anything; generated markdown sanitized | FR-SEC28 |
| T9a | Injection through a bot comment relaying third-party text (dependency changelogs, CI output) | Bot comments quarantined as untrusted data with provenance labels, never treated as more trustworthy than a stranger's comment | FR-SEC28 |
| T9b | Injection through a stranger's comment on a public repository | Non-owner comments excluded from agent context by default on public repositories; quarantined elsewhere | FR-SEC28 |

### Repudiation

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| R1 | Actions cannot be attributed | Audit log of every command, state transition, and external action; AI-attribution commit trailers; the App's identity is distinct from the owner's | FR-SEC10, FR-C3 |

### Information Disclosure

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| I1 | Code sent to a provider not allowed for its tier | Tier enforcement at the gateway covering fallbacks and candidates; tier rules for embeddings, traces, and artifacts | FR-SEC12, FR-SEC13 |
| I2 | Secrets reach sandboxes, prompts, logs, or traces | No credentials in sandboxes; secrets loaded only in platform processes; redaction of known token formats (`ghs_`, `github_pat_`, `sk-ant-`, `sk-`, `AIza`) | FR-SEC4, FR-SEC7 |
| I3 | Data exfiltrated from a sandbox | Network fully disabled during the agent phase; install phase limited to allowlisted registries | FR-SEC4, FR-SEC14 |
| I3a | Worker environment variables inherited by a sandbox | Allowlisted sandbox environment; repository variables supplied as fake values with canaries; worker environment never inherited | FR-SEC23 |
| I4 | Secrets in git history exposed | Shallow clones in sandboxes; pre-run secret scan; history operations performed only by platform code | FR-SEC17 |
| I5 | Personal data in fixtures or dumps sent to providers | Pre-run personal-data scan for `sensitive` and `local_only` tiers blocks the run and reports findings | FR-SEC17 |
| I6 | Secrets leaked during development (Claude Code sessions) | Secrets stored outside the project directory; deny rules plus a `PreToolUse` hook; platform API key kept out of the shell environment | FR-SEC7 |
| I7 | Private code published in portfolio materials | Reports for `private` and more restricted tiers publish aggregates only | FR-SEC13 |
| I8 | Canary secrets escape | Canary detector scans outgoing prompts, comments, and PRs and halts the run on any match | FR-SEC20 |
| I9 | Sensitive data files in a repository reach an agent | Excluded paths listed in the platform repository; substitution with same-schema synthetic files (or omission) before the tree enters the sandbox; excluded from embeddings and briefs; onboarding verifies tests pass with substitutions | FR-SEC22 |
| I10 | Database backup exposes every repository's code and transcripts | Client-side encryption via restic before offsite copy to Cloudflare R2, so the provider holds ciphertext only; secrets excluded from backups; repository key held away from the backed-up machine; full disk encryption on worker machines | FR-SEC30 |
| I11 | Compromised sandbox base image affects every attempt | Images built by CI in the platform repository, published to GitHub Container Registry, pulled by digest rather than tag, rebuilt and scanned weekly, with digest updates arriving as reviewed PRs | FR-SEC30 |

### Denial of Service

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| D1 | Runaway agent loops or spend | Step limits; token budgets; per-attempt, per-feature, and monthly caps that pause work | FR-SEC9 |
| D2 | Sandbox resource exhaustion (fork bombs, disk filling) | Process, memory, CPU, disk, and time limits; orphan reaper | FR-SEC4, NFR-R3 |
| D3 | Webhook flooding | Immediate acknowledgment; events from non-owners dropped early; per-sender rate limiting | FR-SEC1, NFR-P1 |

### Elevation of Privilege

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| E1 | Sandbox escape | Non-root user; all Linux capabilities dropped; `no-new-privileges`; no Docker socket; no privileged mode; read-only root filesystem where feasible; gVisor on Linux workers | FR-SEC4 |
| E2 | Manipulated agent misuses tools | Per-role tool allowlists; read-only roles; tool arguments validated (file paths confined to the workspace) | FR-SEC3 |
| E2a | Symlink escape or check-then-write race from the workspace | File tools operate through a workspace root handle enforcing containment at the syscall level; absolute paths, `..` segments, and encoded traversal rejected rather than sanitized; sensitive in-workspace paths (`.git` internals, substituted fixtures) denied; rejections logged and repeated rejections flag the attempt | FR-SEC29 |
| E3 | Agent obtains GitHub write access | Tokens exist only in platform processes; minimal App permissions (contents, pull requests, issues); never `workflows` or administration | FR-SEC5, FR-SEC15 |
| E4 | Lateral movement between machines | Tailscale access rules limit each device to required ports; workers run as a dedicated non-admin OS user; only the webhook endpoint is public | FR-SEC18 |

### Human Factors

| ID | Threat | Mitigations | Requirements |
|---|---|---|---|
| H1 | Plans approved or PRs merged without real review | Risk summary at the top of plan comments; risky plans require `/approve --accept-risk`; PR descriptions include a security-relevant changes section | FR-SEC19 |

## Model Context Protocol (MCP)

### Not used for agent tools

The platform's agents use a small set of Go functions executed by the worker, not MCP servers. An MCP server is a separate process holding its own credentials and network access, which would place capabilities on the agent side of the sandbox boundary and contradict the principle that agents propose while platform code acts. It would also import tool poisoning (malicious instructions in tool descriptions, which sit in a part of the model's context users do not inspect) and rug pulls (a server changing a tool description after approval).

If MCP is reconsidered later, all of the following are required:

- The server runs on the worker side, never inside a sandbox
- Version and hash pinned; no `@latest` resolution at runtime
- Tool descriptions recorded at approval and re-verified at every startup; a changed description fails the run
- Tokens scoped read-only wherever the server only reads
- Server output treated as untrusted content, like any repository file
- Tool calls logged the same way as native tools (FR-SEC24)

### Development-time MCP (Claude Code sessions)

An MCP configuration change is effectively a change to which executables the development environment will run, and is reviewed on those terms.

Pre-install checklist:

- Verify the publisher and audit the tools and permissions the server exposes
- Read tool descriptions for injected instructions; scan configurations with a tool such as `mcp-scan`
- Pin versions; never run a server through `@latest`
- Scope tokens narrowly, read-only where possible
- Never point a server at production data, databases, or Premier Health infrastructure
- Require approval for destructive actions rather than allowing all tools
- Review combinations, not just individual servers: one server that reads private data plus one that writes externally forms an exfiltration path even when each is acceptable alone
- Keep tokens out of project-level configuration files, as with `.env`

Reviewed weekly alongside the other periodic checks.

## Verification

### Automated (CI)

- **Policy matrix tests:** every tier × role × provider combination, including fallbacks and candidates; a role absent from a repository's allowlist is denied; the hardcoded platform-repository table cannot be widened by configuration
- **Authorization tests:** valid and invalid webhook signatures; commands from non-owner user IDs
- **Sandbox attack tests:** outbound network calls fail during the agent phase; host paths unreadable; Docker socket absent; process runs as non-root; fork bomb contained; disk quota enforced
- **Gate-weakening fixtures:** attempts that delete tests, skip tests, or modify test configuration are rejected
- **Prompt injection fixtures:** test repositories and issues containing instructions to leak environment variables, modify CI, or push to the default branch; tests assert no forbidden action occurs
- **Canary tests:** fake secrets planted in fixtures and environments; tests fail if any canary appears in prompts, logs, traces, artifacts, comments, or PRs
- **Path escape tests:** symlinks pointing outside the workspace, a directory component swapped for a symlink between validation and write, sibling directories sharing a prefix with the workspace root, absolute paths, encoded and doubled traversal sequences (`....//`), and writes to `.git` internals or substituted fixtures are all rejected
- **Gate integrity tests:** an artifact whose hash does not match a passing gate result is refused by the push path
- **Provenance tests:** a comment posted by the App containing a command string triggers nothing; a bot comment carrying injected instructions does not alter agent behavior; non-owner comments on a public repository fixture do not reach agent context; content containing a forged quarantine delimiter or a spoofed author marker does not escape quarantine

### Periodic (weekly, manual)

- Review GitHub App permissions and installations
- Review active API keys and rotate any older than the rotation period
- Review Tailscale devices and access rules
- Sample the audit log for unexpected actions
- Run the tool call outlier query and review flagged attempts rather than reading successful transcripts
- Review configured MCP servers, their pinned versions, and their token scopes
- Skim the generated access matrix for unexpected role or tier grants

### Periodic (monthly, manual)

- Restore the latest backup snapshot into a scratch database, run verification queries, and drop it

## Incident Response Runbook

1. **Stop:** trigger the kill switch.
2. **Read the alert:** the alert issue states what automatic action was already taken and which artifacts hold the evidence. If GitHub is implicated, read the local append-only alert log on the central machine instead.
2. **Revoke and rotate:** generate a new GitHub App private key and delete the old one (installation tokens expire within an hour); rotate model API keys; remove any suspected machine from Tailscale.
3. **Investigate:** review the audit log and the App's GitHub activity (pushed branches, comments, PRs).
4. **Clean up:** close suspicious PRs and delete suspicious branches.
5. **Assess exposure:** determine what data crossed which boundaries, by tier. If personal data from a sensitive repository may have been exposed, check what obligations apply; organizations processing personal data in Uganda may have duties under the Data Protection and Privacy Act, 2019. Confirm with a qualified advisor.
6. **Learn:** write a postmortem as a decision record and add a regression test for the failure.

## Accepted Residual Risks

- A manipulated agent can still produce plausible but subtly harmful code. Deterministic gates and owner review are the final defense.
- Provider handling of data is governed by contractual terms, not technical controls.
- Internal services behind Tailscale rely on network-level access control rather than per-service authentication. Acceptable for a single owner; revisit before adding any other users.
- GitHub, Cloudflare, Tailscale, and model providers are trusted third parties.
- The `local_only` tier has substantially lower model capability.
- Personal-data scanning is pattern-based and can miss data.
- Under `reset_in_place` gating, the agent's environment (dependency trees, caches, global configuration) is not rebuilt, so tests could pass for reasons unrelated to the diff. Mitigated by `clean_room` gating on sensitive repositories and flagged attempts.
- Alert delivery depends on GitHub. If GitHub access is itself compromised or unavailable, alerts are recorded in the local append-only log but not delivered. A second channel is not implemented.

## Security Controls by Milestone

**Milestone 1**
- Webhook signature verification and owner authorization by numeric user ID
- Tier enforcement at the gateway and no-tier-no-run, even with a single provider
- Hardened sandbox defaults; network disabled during the agent phase
- Secrets only in platform processes; development-time secret controls
- Minimal GitHub App permissions without `workflows`; branch rules without App bypass
- Step limits, budgets, audit log, and kill switch

**Milestone 2**
- Canary tokens and detector
- Prompt injection fixtures and sandbox attack tests in CI
- Log and trace redaction
- Pre-run secret and personal-data scans; shallow clones
- Aggregate-only reporting rules

**Milestone 3**
- Tier filtering across all providers, fallbacks, candidates, and judge degradation
- Dependency-addition tool with checks

**Milestone 4**
- gVisor on Linux workers
- Tailscale access rules and dedicated worker OS users
- Tier rules for remote artifact storage
