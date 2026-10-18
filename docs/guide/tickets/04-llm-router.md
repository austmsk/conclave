# Ticket 4 — LLM router and fake provider

**Status:** merged as PR #4 (`0a71acd`) · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-4--llm-router-and-fake-provider) · **Requirements:** FR-I2, FR-SEC12, FR-SEC13, FR-SEC9 · **Review list:** RL-17, RL-6

## In one paragraph

The only way Conclave talks to a model. `internal/llm` holds a router that, for every request, works out which repository the code belongs to and whether its sensitivity tier allows the chosen provider. It also checks whether the role may run there and whether the call fits the token budget. Only then does the request go out. Every billed reply is recorded in the cost ledger exactly once. There is one real adapter, Anthropic, written on `net/http`, and a fake replay provider so no test ever calls a real model.

## Why it exists

Repository code goes into every prompt, so the decision about which provider may see it has to be made in exactly one place, and that place must fail closed (`RL-17`). Each provider expresses tool calls differently, and an adapter bug shows up as strange agent behaviour rather than an obvious error (`RL-6`). That is why adapters normalise to one shape and are tested against the wire format. Budgets and cost records exist so a looping or manipulated agent can only waste a bounded amount (`FR-SEC9`).

## How it works

**Setup, at startup.**

- `LoadConfig` reads `deploy/router.yaml`: models with prices and output limits, a route per role (main model plus fallbacks), and rate limits.
- `LoadRepoAccess` reads `deploy/repos.yaml`: each repository's tier, role allowlist, and, for `sensitive`, its approved providers.
- Both decode strictly (unknown fields are errors) and ship placeholders that fail validation, so nothing runs on made-up prices.
- `llm.New` refuses a config that routes to a provider with no registered adapter.

**Getting a scoped router.** Callers never call `Router` directly. `Router.Scope(ctx, Scope{RepoID, FeatureRequestID, AttemptID, KeyPrefix, TokenBudget})` returns a `*Scoped`, which implements `contracts.Router`. A scope with no repository, no key prefix, a non-positive budget, or IDs that aren't UUIDs is refused. When `AttemptID` is set, the starting spend is read from the ledger, so a retried activity doesn't get a fresh budget.

**One call: `Scoped.Complete`** (`router.go`):

1. **Same repository.** A request naming any other repository than the scope's is refused (`ErrRepoMismatch`).
2. **Authorize.** The request needs a role, a prompt version, and messages. The repository's policy is looked up, the role must be on its allowlist, and the role must have a route. All of this happens before any provider is involved.
3. **Reserve budget.** The input tokens are estimated, the output is capped at the model's limit (or the request's `MaxTokens`), and both are reserved from the budget. Concurrent calls can't all spend the same headroom.
4. **Walk the fallback chain.** For each model in turn:
   - if the tier doesn't allow its provider, **stop** with `ErrTierDenied`; it is never skipped
   - otherwise wait for the rate limiter, then call the adapter
   - move to the next model only if this one was *unavailable*. Any other error ends the request.
5. **Charge and record.** Whatever the provider billed is recorded, even if the adapter then rejected the reply. A reply with no usage is charged the full reservation. Negative counts count as zero. The cost event's key is `prefix/provider/replyID`, so recording the same reply twice counts once.
6. **Settle.** The reservation is swapped for actual spend, and the reply is returned with its provenance: role, prompt version, model, provider.

**The tier table is code.** `policy.go` hard-codes the tiers: `public` allows any known provider, `private` allows Anthropic, OpenAI and Google, and `local_only` allows Ollama. Configuration can't widen them. Only `sensitive` reads a list from config, and that list may name only known providers.

**Who decides the tier.** `deploy/repos.yaml` is authoritative. `DirectoryRepos` looks up the repository's database record by ID, finds its config by full name, and refuses with `ErrTierMismatch` if the two disagree. So tampering with `repos.tier` in the database blocks the repository rather than widening it.

**Errors.** `IsPermanent(err)` says whether a retry could help. Tier, role, config and budget errors, and unsupported replies, are permanent. Unavailability, rate limits, and malformed replies are not. A test checks that every sentinel error is classified. `RateLimitError` carries the provider's `RetryAfter`.

**Anthropic adapter** (`anthropic/`):

- The key is sent only in `x-api-key`. Redirects are never followed, so the key can't be forwarded to another host.
- Provider error messages are dropped, keeping only the status and type, so a reply can't smuggle text into logs.
- A 10-minute default timeout. HTTP statuses map onto the router's typed errors.

**Fake provider** (`faketest/`). It replays a recorded script turn by turn and checks each request: the model, the conversation length, and that every tool call in the last reply has a tool result in this request. A test that drifts from its recording fails instead of getting a reply meant for a different conversation.

## Where to look

| File | What's in it |
|---|---|
| `internal/llm/router.go` | `New`, `Complete`, the fallback chain, `call` |
| `internal/llm/scope.go` | `Scope`, budget reservation and settling |
| `internal/llm/policy.go` | Tier table, `RepoPolicy`, `StaticRepos`, `DirectoryRepos`, policy errors |
| `internal/llm/config.go`, `repoaccess.go` | YAML loading and validation |
| `internal/llm/cost.go` | Cost in USD, `billable`, idempotency keys |
| `internal/llm/limiter.go` | Per-process requests-per-minute and tokens-per-minute limits |
| `internal/llm/permanent.go` | `IsPermanent` |
| `internal/llm/embed.go` | `Embed`, under the same checks (no embedding model configured yet) |
| `internal/llm/anthropic/` | The Anthropic adapter |
| `internal/llm/faketest/` | The replay provider |
| `internal/store/costs.go`, `repos.go` | The Postgres cost ledger and repository directory |
| `deploy/router.yaml`, `deploy/repos.yaml` | Owner configuration with failing placeholders |

## What the tests prove

| Guarantee | Test |
|---|---|
| Every tier × provider and role × repository combination | `TestPolicyMatrix`, `TestRoleAllowlistMatrix`, `TestEmbedTierMatrix` |
| Config can't widen a tier; a tampered DB tier is refused | `TestConfigCannotWidenTier`, `TestTamperedTierIsRefused` |
| A disallowed fallback stops the request; fallback only on unavailable | `TestDisallowedFallbackIsNotSkipped`, `TestFallbackOnlyOnUnavailable` |
| Unknown or misconfigured repositories fail closed | `TestRepoPolicyFailsClosed`, `TestDirectoryReposFailsClosed` |
| Budgets hold under concurrency and across retries | `TestConcurrentCallsStayInBudget`, `TestRetriedScopeKeepsBudget` |
| Rejected, zero-usage and negative-usage replies can't dodge the budget | `TestRejectedReplyIsCharged`, `TestZeroUsageRepliesExhaustBudget`, `TestNegativeUsageDoesNotCreditBudget` |
| The same reply is recorded once; a recording failure fails the call | `TestSameReplyRecordedOnce`, `TestCostRecordingFailureFailsTheCall` |
| Every sentinel is classified permanent or not | `TestEverySentinelIsClassified` |
| The adapter never follows a redirect; drops provider messages | `TestRedirectIsNotFollowed`, `TestErrorMessageIsDropped` |
| A multi-turn tool conversation replays through the router | `TestReplayMultiTurnThroughRouter` |
| The shipped YAML placeholders fail validation | `TestDeployedConfig`, `TestDeployedRepoAccess` |

## Left open

Most of these are owed by ticket 8a or 8:

- **Two live scopes can spend twice the budget.** The budget is seeded from the ledger once, then tracked in memory, so an abandoned activity and its retry each see only their own calls. Ticket 8a fixes this with database reservations.
- **No per-feature-request or monthly cap yet** (`FR-SEC9`). This is ticket 8a.
- **Attribution is optional.** `AttemptID` and `FeatureRequestID` may be empty. Ticket 8a makes them required on budgeted scopes.
- **Nothing is wired into `cmd/worker`.** Ticket 8 wires the config loaders, `DirectoryRepos`, and the ledger, turns `IsPermanent` into non-retryable Temporal errors, and uses `RetryAfter`.
- The rate limiter is per process, not global. It also blocks without heartbeating, so ticket 8 must heartbeat around it.
- The `00004` grant still lets `conclave_app` UPDATE `repos.tier`. It is caught by the mismatch check, but not narrowed or audited.
- Decision records: [0006](../../decisions/0006-prompt-version-on-model-requests.md) (prompt version), [0007](../../decisions/0007-yaml-library.md) (YAML library), and [0008](../../decisions/0008-provider-adapters-on-net-http.md) (`net/http` over SDKs, proposed). Still unwritten: the scope and ledger design, and the rate-limit defaults.

## Check your understanding

1. A role's main model is unavailable and its fallback's provider is disallowed by the tier. What happens, and why not try a third model?
2. Why is the tier table code rather than config, and why is `sensitive` the exception?
3. Someone changes a repository's `tier` in the database from `private` to `public`. What does the next request do?
4. An adapter rejects a reply as malformed, but the provider billed for it. Is the spend recorded, and why?
5. Why is the cost event keyed on the provider's reply ID rather than the call's position in the conversation?
6. How can two scopes for the same attempt overspend today, and what does ticket 8a change?
