# Milestone 1 exit checklist

One row per requirement ID that Milestone 1 claims, with the ticket that owns it and the test that proves it. A ticket's PR description names the test for every ID it claims and updates this file (`CLAUDE.md`, Session discipline). The ticket reviewer checks the PR against this table. Rows for unmerged tickets say *named in the PR*; the ticket session fills them.

The point of the table is the last section: what Milestone 2 must find in place when Milestone 1 closes. Tickets 8-pre, 8, 8b, 9 and 10 are kept in scope by that section, not by this table alone.

Test names are functions; `pkg:` gives the package under `internal/` unless stated. Where a requirement is enforced by tooling rather than a test, the row says so.

## Merged tickets

| ID | Requirement | Ticket | Proof |
|---|---|---|---|
| NFR-M1, NFR-M8 | Boring code, enforced limits | 1 | `make lint` with gocyclo, gocognit, funlen, nestif; CI job `check` |
| NFR-PO1 | macOS and Linux | 1 | CI on ubuntu; owner runs on macOS |
| FR-SEC10 | Audit log of every transition | 2 | store: `TestAppRoleCannotAlterAppendOnlyRows`, `TestAuditTriggerResistsBypass`, `TestSessionScopedActorIsStillAudited`, `TestTransitionRecordsReason` |
| RL-13 | Compare-and-set transitions | 2 | store: `TestTransitionCoversEveryLegalMove`, `TestTransitionRefusals`, `TestTransitionUnderConcurrentUpdate`, `TestApprovePlanUnderConcurrentApproval`; contracts: `TestCanTransitionExhaustive`, `TestSchemaAcceptsExactlyTheContractStates` |
| NFR-M7 | Validated plain configuration | 2, 4, 5, 6, 7, 8a | llm: `TestLoadConfigRejects`, `TestDeployedConfig`, `TestLoadRepoAccessRejects`; agent: `TestLoadToolchains`; github: `TestLoadConfig`; gate: `TestLoadConfig_Invalid`, `TestDeployedGatesConfigIsPlaceholder`; llm: `TestLoadConfigRequiresBudgets` |
| FR-SEC4 | Sandbox isolation | 3 | sandbox/docker: `TestAttackOutboundNetworkFailsWithNetworkNone`, `TestAttackProcessIsNonRootWithoutCapabilities`, `TestAttackDockerSocketAbsent`, `TestAttackForkBombIsContained`, `TestAttackDiskQuotaHolds`, `TestAttackHostPathsUnreadable`, `TestHostConfigIsHardened` |
| FR-SEC21 | No git credentials in the sandbox | 3, 6 | sandbox/docker: `TestAttackNoTokenReachesTheSandbox`, `TestAttackUnpreparedTreeIsRefused`; github: `TestCloneLeavesNoToken` |
| FR-SEC23 | Allowlisted environment only | 3 | sandbox/docker: `TestAttackWorkerEnvironmentNotInherited`, `TestBuildEnvIsFixedPlusAllowlistOnly` |
| NFR-R3 | Guaranteed cleanup (reaper half) | 3 | sandbox/docker: `TestReapRemovesOrphansOlderThanTheGivenAge`, `TestReapSweepsOtherNamespacesOnlyPastStaleAge`, `TestCreateFailureLeavesNothingBehind` |
| FR-I2 | Providers behind one router | 4 | llm: `TestNewRequiresAdapterForEveryProvider`, `TestReplayMultiTurnThroughRouter`, `TestFallbackOnlyOnUnavailable` |
| FR-SEC12 | No tier, no run | 4 | llm: `TestRepoPolicyFailsClosed`, `TestDirectoryReposFailsClosed`, `TestNewRejectsBadStaticRepos`; store: `TestRetieredRowIsRefused` |
| FR-SEC13 | Tier enforced on every request and fallback | 4 | llm: `TestPolicyMatrix`, `TestFallbackMatrix`, `TestDisallowedFallbackIsNotSkipped`, `TestTamperedTierIsRefused`, `TestConfigCannotWidenTier`, `TestEmbedTierMatrix` |
| FR-SEC9 | Per-attempt budget | 4 | llm: `TestBudget`, `TestRetriedScopeKeepsBudget`, `TestZeroUsageRepliesExhaustBudget`, `TestNegativeUsageDoesNotCreditBudget` |
| FR-SEC9 | Feature-request and monthly caps | 8a | store: `TestConcurrentAttemptsStayInFeatureRequestCap`, `TestConcurrentCallsStayInMonthlyCap`, `TestMonthIsUTC`, `TestReservationExpiresOnDatabaseClock`, `TestConcurrentScopesStayInAttemptBudget`; llm: `TestCapRefusals`, `TestScopeRequiresAttribution`, `TestSecondScopeSeesInFlightCall` |
| NFR-P6 | Cost tracked per call | 4, 8a | llm: `TestCostRecording`, `TestSameReplyRecordedOnce`, `TestRetriedActivityRecordsItsOwnSpend`; store: `TestSettleIsIdempotent` |
| FR-SEC3 | Per-role tool allowlist | 5 | agent: `TestOfferedToolsIntersectAllowlist`, `TestDisallowedCallNeverReachesSandbox`, `TestReadOnlyStripsMutatingTools` |
| FR-SEC24 | Every tool call recorded by the worker | 5 | agent: `TestEveryToolCallRecordedBeforeNextRequest`, `TestRecordFailureFailsClosed`, `TestCanaryNotInAudit`; store: `TestToolCallsReachAuditTable`, `TestRecordToolCallIsIdempotent` |
| FR-SEC29 | Path validation, reject never sanitize | 5 | agent: `TestAttackSiblingPrefix`, `TestAttackToolsRefuseSymlinks`, `TestSearchDoesNotFollowSymlinkOut`, `TestReadFileRefusals`, `TestWriteFileRefusals`, `TestDenylistComponentAware`; sandbox/docker: `TestAttackSwapBetweenValidationAndWrite` |
| FR-I4 | Toolchain commands from configuration | 5 | agent: `TestConfiguredCommandArgvExact`, `TestRunCommandRefusesArguments`, `TestLoadToolchains` |
| FR-I1, FR-SEC15 | GitHub App, least permissions | 6 | github: `TestInstallationTokenObtainedAndUsed`, `TestTokenWithExcessPermissionsRefused`, `TestTokenScopedToOneRepository` |
| FR-SEC1 | Only the owner's numeric ID commands | 6 | github: `TestLabelByNonOwnerIgnored`, `TestCommandFromNonOwnerIgnoredAndLogged`, `TestCommandInAppCommentTriggersNothing` |
| FR-SEC2 | Webhook signatures verified | 6 | github: `TestWebhookRejectsBadSignature`, `TestWebhookVerifiesBeforeParsing`, `TestNewReceiverRejectsWeakSecret` |
| FR-SEC5, FR-SEC16 | Platform-only push, `agent/` branches only | 6 | github: `TestPushOnlyToAgentBranches`, `TestPushLeaseRefusesStaleExpected`, `TestPushRepeatedIsIdempotent`, `TestCreatePullRequestHeadMustBeAgentBranch` |
| FR-SEC7 | No secrets in logs or errors | 6 | github: `TestNoTokenInLogsOrErrors`, `TestAPIErrorsCarryNoSecrets`, `TestPushErrorRedacted`, `TestSecretFormatsRedacted` |
| FR-SEC28 | Provenance and quarantine | 6 | github: `TestClassifyProvenance`, `TestQuarantineForgedDelimiters`, `TestQuarantineSpoofedAuthorMarkerStaysInside`, `TestAdmitPublicTierExcludesNonOwner`, `TestOutgoingBodiesSanitized` |
| NFR-P1 | Webhook acknowledged within two seconds | 6 | github: `TestWebhookAcksBeforeDispatch`, `TestWebhookQueueFullReturns503` |
| FR-A1 | Deterministic gates before any judging | 7 | gate: `TestGates_EachGateFailingFixture`, `TestGate_StaticFailureSkipsSandbox`, `TestTestsExitZeroWithoutReportFails` |
| FR-A3 | Gate weakening rejected | 7 | gate: `TestGate_RejectsDeletedTest`, `TestGateWeakening`, `TestCheckWeakening_Fixture` |
| FR-SEC8 | Protected paths | 7 | gate: `TestCheckProtected`, `TestCheckProtected_Fixture` |
| FR-SEC25 | Execution-surface changes flagged | 7 | gate: `TestExecutionSurface_Flags`, `TestCheckExecSurface_RepoAddition` |
| FR-SEC27 | Gate-to-push integrity | 7 | gate: `TestHashBindsBase`, `TestGate_EveryResultCarriesCheckedHash`, `TestAttackPostFreezeMutationIgnored`, `TestGate_NotReproducedFails`, `TestVerifyTamperedBlob`, `TestGate_EnvironmentFailuresAreErrored`; store: `TestArtifactHashWriteOnce`, `TestGateResultsUpdateDenied` |
| NFR-R2 | Idempotent actions (so far) | 6, 7, 8a | github: `TestCreatePullRequestIdempotent`, `TestPushRepeatedIsIdempotent`; gate: `TestGateTwiceRunsOnce`, `TestGate_PartialRecordRunsAgain`; store: `TestSettleIsIdempotent`, `TestGateResults_RecordIdempotentFirstWriterWins` |

## Unmerged tickets

| ID | Requirement | Ticket | Proof |
|---|---|---|---|
| NFR-R2, NFR-R3, FR-SEC12, FR-SEC30, NFR-M7 | Prerequisites: release by ID, idempotent creates, repo seeding, validated sandbox config | 8-pre | *named in the PR* |
| NFR-R1, NFR-R2, NFR-R3, FR-S5, FR-S12, FR-S19 (pinning) | Workflow survives crash, retry and cancellation; approval pins the base; every problem routes to `needs_attention` | 8 | *named in the PR*; must include the outcome-table test, the canary-not-in-history test, and the traceability join test from `ticket-8-pre-plan.md` |
| FR-S11, FR-S12, FR-SEC10 | `/retry`, invalid-command replies, `/status`, `/help`, status comment, command audit | 8b | *named in the PR* |
| FR-S10, FR-S16, FR-S17, FR-S19 (rebase), FR-A5, FR-A7, FR-SEC27 (push), FR-C3 | PR delivery, bot commits, rebase and re-gate, CI result observed | 9 | *named in the PR* |
| FR-SEC11, FR-SEC31, NFR-R3 (reaper), NFR-M9 | Kill switch, alerts, scheduled reaper, decision records; the 10 trials | 10 | *named in the PR*; trial log under `docs/trials/` |

## Milestone 1 success criteria

| Criterion | Owned by | Evidence |
|---|---|---|
| Labeled issue on Election Tally → PR passing CI, 7 of 10 trials | 10 | `docs/trials/` log: issue, attempt, gate results, PR, CI result, cost, duration per trial. `/retry` does not count as a new trial; a trial is one label. |
| Zero orphaned sandboxes over 10 runs including 2 cancellations | 8-pre, 8, 10 | `store.UnreleasedAttempts` empty after the trials, and `docker ps` filtered by the `conclave.sandbox` label empty |
| Every run traceable in the Temporal UI and the database | 8 | the join test (`feature_requests.workflow_id` → plans → tasks → attempts → gate_results, cost_events, tool_calls, state_transitions) and the workflow ID visible on the feature request row |

## What Milestone 2 must find in place

Milestone 2 builds the evaluation harness: the `test_writer` role and hidden-test split, a replay benchmark of at least 20 tasks from the owner's repositories plus a SWE-bench Verified subset, and a report of resolve rate, cost per resolved task and time per task, with model, prompt and configuration versions recorded and flaky tests excluded (FR-S7, FR-A2, FR-A4, NFR-P6). Each item below is a seam a Milestone 1 ticket must leave, or a deferral it must record. The ticket in brackets owns it.

1. **A task runs without GitHub** [8]. `TaskWorkflow` is a child workflow taking only serializable inputs: task ID, attempt ordinal, base SHA, repo full name. It performs no GitHub call and no delivery. The benchmark driver in `internal/eval` inserts a feature request, plan and task directly through the 8-pre store functions and starts `TaskWorkflow`. The parent `FeatureWorkflow` is the only thing that knows about issues, approval and PRs.
2. **The plan body is the frozen agent input** [8-pre, 8]. `plans.body` holds the `TaskSpec`, the quarantined issue text, the `RepoProfile`, the `Constraints`, and the SHA-256 of each loaded deploy file (`router.yaml`, `repos.yaml`, `toolchains.yaml`, `gates.yaml`, `sandboxes.yaml`). That is the "configuration" Milestone 2's results record.
3. **Provenance, usage and timing on every attempt** [8-pre `FinishAttempt`, 8]. `attempts` carries role, prompt version, actual model and provider, tokens, cost, steps, `started_at`, `finished_at`; `cost_events` are attributed to the attempt (8a). Resolve rate, cost per resolved task and time per task are one SQL query, written and checked in ticket 10 for the trial report, so Milestone 2's generator starts from a query that exists.
4. **Install phase through the contract** [8-pre, 8]. `CreateWithInstall` is on `SandboxProvider`, and `sandboxes.yaml` holds per-repository image digest and install commands, so a Python image and `pip install` for SWE-bench are configuration, not code.
5. **Gate seams, additive** [7 done; 8 and 9 must not restructure]. Hidden tests become an overlay applied after the recorded diff and before the command gates; the flaky-test baseline is a staging of the base without the diff. Both fit ticket 7's re-stream staging without changing `gate.Input`'s existing fields. Ticket 9's rebase-and-re-gate reuses `Gate` unchanged.
6. **Role is data** [5 done; 8 must not special-case the implementer]. Adding `test_writer` is a `RoleSpec`, a prompt, a router route and a `repos.yaml` allowlist entry. The attempt activity takes the role as input rather than assuming `Implementer`.
7. **Trial issues seed the benchmark** [10]. The 10 trial issues are written in the owner-section format (Task, Acceptance criteria, Hints) and saved under `docs/trials/` with their base SHA and outcome, so they are the first replay tasks.
8. **Deliver is a leaf** [8, 9]. Delivery is one activity after the task child completes; nothing in the task phase depends on it, and a `SkipDelivery` flag is not needed because the benchmark never runs the parent.
9. **Decision records exist** [every ticket, 10 checks]. NFR-M9: SDK, activity granularity, timeouts, release by ID, analyze step, deliver stub, reuse policy, deferrals.
10. **Recorded deferrals, so a later session does not treat them as bugs**: restacking (FR-S18a) and branch cleanup (FR-S20) to Milestone 4; OpenTelemetry export, pre-run secret scan (FR-SEC17), excluded-path substitution (FR-SEC22), CI image build and registry publication (FR-SEC30), canary tokens (FR-SEC20) to a ticket after Milestone 1.
