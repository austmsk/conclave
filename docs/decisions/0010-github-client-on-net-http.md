# 0010 — GitHub App auth and REST are written on net/http, not go-github or ghinstallation

**Date:** 2026-10-19
**Status:** accepted

## Context

`ARCHITECTURE.md` listed "GitHub App + `go-github` + `ghinstallation`", and ticket 6 said "App authentication via `ghinstallation`". Neither module was in `go.mod`. Ticket 6 needs:

- one App JWT and the installation-token exchange;
- six REST calls: create a comment, read one, edit one, add a reaction, list pull requests and create one;
- webhook signature verification;
- a git push.

This client is the platform's single choke point for repository writes. It holds the most powerful credential the platform has (RL-7, RL-8).

Three plans were drawn up independently and compared: one on the standard library, one on `go-github` v88 with `ghinstallation` v2.19.0, and one chosen for the smallest attack surface. The library plan's own research turned up these facts:

- `ghinstallation` v2.19.0 requires `go-github` v88 and `golang-jwt/jwt/v4`, and `go-github` requires `go-querystring`. That is four new modules. `go-github` would stay pinned behind its current major (v93) for as long as `ghinstallation` requires v88.
- `ghinstallation.HTTPError` and `go-github`'s `ErrorResponse` both keep the `*http.Response`. Its `Request.Header` carries the installation token or the App JWT, so any error that is wrapped rather than converted carries the credential.
- `ghinstallation`'s transport adds `Authorization` again on every round trip. If a redirect is followed, the token goes to the redirect target, even across hosts, where Go would otherwise strip the header.
- `go-github`'s `ValidatePayload` accepts the SHA-1 signature header. It also skips verification when both the secret and the signature are empty.
- `go-github`'s `IssueComment` has no `performed_via_github_app` field. FR-SEC28 needs that field to tell an App-authored comment apart.

Containing all of that would have taken more code than writing the client. That is the same reasoning as [0008](0008-provider-adapters-on-net-http.md).

## Decision

`internal/github` is written on `net/http`, `encoding/json` and the standard library's crypto packages:

- The App JWT is RS256: `crypto/rsa` with `SignPKCS1v15` over SHA-256.
- Each installation token covers one repository (`repository_ids`) and a fixed permission set: contents, issues and pull requests write, metadata read. GitHub's reply is checked, and a token carrying `workflows`, `administration`, or any other repository is thrown away unused.
- The REST client follows the anthropic adapter:
  - redirects are refused;
  - error bodies are read with a bound and then discarded;
  - errors have fixed text: operation, status and request ID, unwrapping only to a sentinel;
  - transport causes are dropped.
- Webhooks are verified on `X-Hub-Signature-256` only, with `hmac.Equal`. The handler refuses to start with a secret shorter than 32 bytes.
- Push runs git with a scrubbed environment. The token goes in an extra header scoped to the repository URL, and hooks and redirects are off. Clone goes through ticket 3's `sandbox.Clone`, which now also refuses redirects and scopes its header to the clone URL's host.

`ARCHITECTURE.md` and ticket 6's text now say so.

## Consequences

- No module enters the build graph for GitHub access. No library release can change how an error formats underneath the leak tests (`TestAPIErrorsCarryNoSecrets`, `TestNoTokenInLogsOrErrors`).
- The project owns RS256 signing, which tests check against the public key. It also owns token expiry and clock skew (`iat` is set 60 seconds in the past), and keeping up with API changes.
- Features a library would give for free are written when a ticket needs them: pagination, GraphQL (ticket 9 needs `markPullRequestReadyForReview` to take a pull request out of draft), and the checks API. Retries are not done in the client; Temporal's activity retry policy owns them, as in 0008.
- The REST surface is small and every call is listed in `internal/github`, so reviewing what the App can do to a repository means reading one package.

## Amendment, 2026-11-27: the permissions and calls added since ticket 6

Reviewed after Milestone 1 closed and accepted. The reasons above still hold, and the client stays hand-written on `net/http`. Its scope has grown since this record was written:

- **Run tokens also ask for `checks: read`.** Ticket 9 polls the repository's CI through the checks API ([0014](0014-pull-request-delivery.md)). A run token now asks for contents, issues and pull requests (write), and checks and metadata (read). The set is fixed in code (`tokenPermissions` in `internal/github/appauth.go`), so no configuration can widen it. A reply granting more than was asked is still refused, and `workflows` and `administration` are refused even if a later edit asks for them. Checks are read, never written.
- **Alerts use a second, narrower token.** Ticket 10a opens alert issues in a separate operations repository with a token limited to `issues: write` and `metadata: read` (`alertTokenPermissions`, [0015](0015-kill-switch-alerts-and-reaper.md)). No run token reaches that repository.
- **The six REST calls are now twelve.** Ticket 6's six are unchanged. Six were added:
  - listing an issue's comments, to find the App's own marked comments so that a retry edits rather than posts again ([0013](0013-commands-and-status-surface.md));
  - reading a pull request, listing a commit's check runs, and marking a pull request ready for review, which is a GraphQL call ([0014](0014-pull-request-delivery.md));
  - creating and listing alert issues ([0015](0015-kill-switch-alerts-and-reaper.md)).

Every call is still in `internal/github`, so reviewing what the App can do to a repository still means reading one package. All of them go through the same request path (`send` and `do` in `internal/github/api.go`), which keeps the fixed error text and refuses redirects.

**Known gap.** The leak tests have not kept up. `TestAPIErrorsCarryNoSecrets` drives only ticket 6's calls (its `restCalls` table: create, edit and react to a comment, and create a pull request, which covers listing). The alert calls have their own test, `TestAlertErrorsCarryNoSecrets`. Listing an issue's comments, listing check runs and marking a pull request ready are covered only through the shared request path, not by a test that fails each of them and checks the error for credentials. Adding them to `restCalls` is a small test-only change for a later ticket.
