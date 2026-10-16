# 0006 — Model requests carry a prompt version

**Date:** 2026-10-16
**Status:** accepted

## Context

Every result is meant to record the model version, prompt version and configuration that produced it (scope-and-requirements.md, evaluation goals). Ticket 2 put `PromptVersion` on `contracts.RoleSpec` and on `contracts.Provenance`, but not on `contracts.Request`, the one value that actually passes through the router. The router builds a response's `Provenance`, and it had no way to learn the prompt version, so the field in `Provenance` could never be filled in. Outputs from before and after a prompt change could not be told apart, and that is the comparison the evaluation work depends on.

Ticket 4 did not ask for a contract change. The gap was found in the ticket's first review, and I approved the change separately, as its own commit (abcc98b), rather than letting it go in as a side effect.

## Decision

`contracts.Request` gains `PromptVersion string`. The caller copies it from the role's `RoleSpec`. The router rejects a request without it (`ErrInvalidRequest`, before any provider is called) and copies it into `Response.Provenance`.

The field is required, not optional. If it were optional, the cheapest thing for a caller to do would be to leave it out, and then most of the provenance records would be blank. A blank value tells you nothing, while a refused request gets noticed in the first test.

An alternative was for the router to look up the version itself from the role. I rejected it because the router routes by role but does not own the role's instructions. The text in `System` is whatever the caller built, so only the caller knows which version of the prompt it is sending.

## Consequences

Every caller of `contracts.Request` must supply a prompt version, or its requests fail. Today the only callers are tests, so nothing was broken. The activity in ticket 8 is the first real caller and must take the value from `RoleSpec`.

The version is self-reported. The router checks that a version is present, not that it matches the text in `System`. A caller that edits a prompt without bumping its version produces wrong provenance, and nothing here catches that. If this becomes a problem, the fix is to derive the version from a hash of the static instructions, not to trust a label.

Tests guard the behaviour: the policy matrix asserts the version reaches `Provenance` on every allowed route, and `validateRequest` has a case for the empty version.
