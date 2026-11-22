# 0008 — Provider adapters are written on net/http, not vendor SDKs

**Date:** 2026-10-17
**Status:** accepted

## Context

`ARCHITECTURE.md` originally listed the official Anthropic, OpenAI and Google Go SDKs, plus Ollama, as the model stack. Ticket 4 needed the first adapter, for Anthropic's Messages API, behind the role router. The options were the vendor SDK or a hand-written client on `net/http`.

Several requirements bear on that choice:

- NFR-M1 puts the standard library first, and NFR-M4 requires a decision record for every new dependency. Each vendor SDK is a new module with its own transitive dependencies, and following the stack table would eventually mean three of them.
- The adapter is a security boundary. It holds the API key and handles provider errors that can quote repository content back. FR-SEC13 and the canary tests require that neither reaches an error, a log or a trace. With an SDK, that guarantee depends on how its error types format themselves, which can change in any SDK release.
- RL-6 requires adapters to normalize provider behaviour. That covers billed usage on rejected replies, unknown stop reasons and content blocks treated as errors rather than ignored, and the tool-result ordering. The router decides on fallback from the adapter's typed errors (`ErrUnavailable`, rate limit, `ErrAuth`), so the adapter has to map HTTP status and body precisely.
- The fake replay provider and the golden request test (`internal/llm/anthropic/testdata/request.golden.json`) pin the exact wire format. An SDK adds a layer between the code and the request body that the golden file would also have to track.

## Decision

Provider adapters are written directly on `net/http` and `encoding/json` and implement `llm.Provider`. The Anthropic adapter (`internal/llm/anthropic`, about 370 lines) is the first. OpenAI, Google and Ollama adapters will follow the same pattern when a later milestone needs them. `ARCHITECTURE.md` reflects this.

The adapter:

- sends the key only in the `x-api-key` header
- drops the transport error's cause and the API error message (`TestErrorMessageIsDropped`)
- redacts the key from every error
- maps 401/403/429/5xx/529 to the llm package's typed errors

The golden file pins the wire format.

## Consequences

- No new modules enter the build graph for model access, and no SDK release can change error formatting or wire format underneath the redaction tests.
- The project owns the wire format. When a provider changes its API (new content block types, new stop reasons, new usage fields), the adapter must be updated by hand. Because unknown values are errors rather than silently ignored, such a change fails loudly, at the cost of an outage until the adapter is updated.
- Features an SDK would provide for free have to be written when needed. Examples are streaming, automatic retries with backoff, and request signing for cloud-hosted variants (Bedrock, Vertex). Retries are deliberately not done in the adapter: Temporal's activity retry policy and the router's fallback chain own them.
- Each future adapter costs roughly one ticket's worth of code and tests, not a thin wrapper around an SDK.
- Adding a provider also means adding it to the in-code tier table in `internal/llm/policy.go`. This is deliberate, so configuration cannot widen a tier, and it is the one place where FR-I2's "no other code changes" does not hold literally.

I approved this change during ticket 4's review. This record is the NFR-M9 justification for the change to `ARCHITECTURE.md`.

## Amendment, 2026-11-21: what the Anthropic API now does that the adapter does not, and the rules for a local adapter

Reviewed after Milestone 1 closed. The reasons above still hold: no key or quoted content reached a log or an error in the trials, and the redaction tests run on every build. The adapter stays hand-written on `net/http`. Model gateways and an MCP layer in front of providers were considered and rejected for the reasons in the next paragraph.

**Why not a gateway or MCP.** MCP connects an application to tools and data; it defines no call to a model provider, so an MCP server in front of a provider would still contain an adapter, and add a process and a hop. A model gateway (one API in front of many vendors) would move the tier check and the API key out of the worker, and flatten the vendor features listed below. FR-SEC13 requires the tier check on every request and every fallback, in Conclave's router. For `local_only` that matters most: the check knows only a provider's name, and a proxy behind the name could send the request anywhere.

The API has moved since this record was written. The trials ran the implementer on `claude-fable-5-2`, and the local router now also routes to `claude-opus-5-5`. Six items follow. The first five are Milestone 2 work, and each is an adapter change, as this record expected.

1. **Effort is never sent, so each model runs at its default.** `output_config.effort` (`low` to `max`) defaults to `high` on most current models but to `medium` on Claude Opus 5.5, so a route moved from Opus 5 to Opus 5.5 silently runs one level lower. Each route sets effort explicitly, and each attempt records the effort it ran at, next to the model it records today. Recording it changes `contracts.Provenance` and the `attempts` table, which are reviewed by hand. Without it, Milestone 2's comparison between models is a comparison at different effort.

2. **Thinking blocks are dropped, not replayed.** The request sets no thinking option, but Fable 5.2 and Opus 5.5 think regardless; they cannot turn it off. The adapter discards every `thinking` and `redacted_thinking` block, so each turn of the tool loop starts without the model's earlier reasoning. Requests succeed this way, as the ten trials show, but it plausibly costs quality and tokens on long loops. The API's guidance is to pass the blocks back unchanged. A block is tied to the model and the conversation that produced it, and one another model cannot read is dropped by the API, unbilled. Replaying needs the block's `signature`, which the adapter's block type does not carry, and a `contracts.Message` change, reviewed by hand. Milestone 2 decides whether to replay; the code comment saying thinking "is not requested" is corrected in the same change as this amendment.

3. **A refusal is an unknown stop reason today.** Current models can end a reply with `stop_reason: "refusal"` (HTTP 200) and a `stop_details` category such as `cyber`. The adapter fails it as `ErrUnsupportedReply`, a permanent error: the router does not fall back, the attempt ends `errored`, and the reason recorded is a provider fault rather than a refusal and its category. It becomes its own typed outcome, routed to `needs_attention` with the category as the reason, and counted separately in Milestone 2's taxonomy.

   **Server-side fallbacks are not used.** The API can now re-serve a refused request on another model within the same call (the `fallbacks` parameter, and the SDKs' refusal-fallback middleware). That model is chosen outside Conclave's router, so the tier check would not run on it. A refusal comes back to the router, and any retry on another model goes through the router's own fallback chain and tier check.

4. **Requests are not streamed, so a call is bounded at ten minutes.** That is the API's limit for a non-streaming request, and the adapter's `DefaultTimeout` and the budget reservations are both sized to it (`llm.MaxCallDuration`). The router allows up to 128,000 output tokens per call. Trial turns of about 23,000 tokens took one to three minutes, so a turn near the cap could pass ten minutes, time out, and be reported as unavailable. Streaming lifts the limit. It is written before effort is raised or longer turns are allowed. Reservations then need a bound of their own, since a streamed call has no fixed ten-minute end.

5. **Prompt caching is not requested.** The adapter already decodes the cache fields of `usage` and records them, but sends no `cache_control`. The agent loop resends a growing conversation each turn, so caching the stable prefix (tools and system prompt) is the largest cost lever for Milestone 2's runs, which multiply tasks by seeds by models. Cost per resolved task in the trials was $7.34.

6. **Rules for a local adapter.** `local_only` repositories may use only the `ollama` provider (`internal/llm/policy.go`), but no Ollama adapter exists yet. The ticket that writes one:
   - **validates the endpoint as local:** loopback, or an address on the owner's private network, and nothing else. Otherwise an `ollama` provider pointed at a remote host passes the `local_only` check by name.
   - **reports token counts.** A reply that reports no usage still runs the budget out (`TestZeroUsageRepliesExhaustBudget`), so a provider cannot run unmetered. Ollama reports prompt and output counts, and the token caps then hold though the price is zero, as the configuration already allows for local providers.
   - **maps tool calls strictly** to `contracts.ToolCall`, failing on anything malformed, as this adapter does. Local models vary in how reliably they call tools.

