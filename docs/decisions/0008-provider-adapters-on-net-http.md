# 0008 — Provider adapters are written on net/http, not vendor SDKs

**Date:** 2026-10-17
**Status:** proposed

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
