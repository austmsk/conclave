# Ticket 6 — GitHub App and webhooks

**Status:** merged as PR #12 (`00ea73e`) · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-6--github-app-and-webhooks) · **Decision:** [0010](../../decisions/0010-github-client-on-net-http.md) · **Requirements:** FR-I1, FR-SEC1, FR-SEC2, FR-SEC5, FR-SEC15, FR-SEC28, NFR-P1 · **Review list:** RL-7, RL-8

## In one paragraph

`internal/github` is Conclave's only path to GitHub. It has two halves, which run in different processes.

**The `Receiver`, in `cmd/server`.** This is the webhook endpoint, the one internet-facing piece. It:
- verifies each delivery's HMAC signature before reading anything else;
- decides from the payload's numeric author IDs whether the owner sent it;
- hands accepted work to a `WorkflowStarter` through a queue, answering GitHub in microseconds.

**The `App`, for the worker.** This is the client that writes to repositories. It:
- signs its own JWT and mints installation tokens scoped to one repository;
- pushes only to `agent/` branches, always with a lease;
- opens pull requests and adopts only the ones it opened itself;
- posts comments and edits only the comments it posted;
- adds reactions.

The package also holds the FR-SEC28 pieces ticket 8 will use when it puts GitHub text in front of an agent:
- `Classify`, which decides where text came from;
- `Quarantine`, which wraps text in a nonce-delimited block;
- `Admit`, which decides whether text may enter an agent's context at all.

There is no GitHub library: everything is on `net/http` and the standard library's crypto ([0010](../../decisions/0010-github-client-on-net-http.md)). The Temporal side of the starter arrives in ticket 8. Until then `cmd/server` logs what it would have started.

## Why it exists

GitHub access is the platform's most powerful capability (RL-7). The design follows from four threats.

- **A forged or replayed delivery** (threat S1).
  - Every delivery is checked against `X-Hub-Signature-256` with `hmac.Equal`, before its body is parsed (FR-SEC2).
  - Duplicates and replays are harmless because the workflow ID is deterministic.
- **Someone other than the owner giving orders** (S2, T9, FR-SEC1, FR-SEC28).
  - Only the owner's numeric user ID can label or command. A username is never used, because usernames change.
  - Anything performed through an App, any bot, and the platform's own comments never authorise anything.
  - The comment body is never consulted for identity.
- **A leaked or over-broad credential** (I2, E3, FR-SEC15, RL-8).
  - The server never holds the App's private key.
  - Tokens cover one repository and a fixed permission set, and never include `workflows` or `administration`.
  - Every secret prints as `[redacted]`.
  - Errors carry fixed text, never GitHub's message.
  - Redirects are refused by the API client, by `Push`, and by ticket 3's `sandbox.Clone`, which this ticket changed to match.
- **A write to the wrong place** (T1, T3, FR-SEC5, FR-SEC16).
  - Pushes go only to `agent/<issue>/integration` or `agent/<issue>/pr-<n>`.
  - The refspec is built inside the package, and every push carries a lease.
  - Pushes come only from a tree `sandbox.VerifyTree` accepts.

NFR-P1 requires an acknowledgement within two seconds, so the handler does no network I/O. It verifies, classifies and enqueues, then returns 202.

## How it works

**Configuration** (`config.go`, `deploy/github.yaml`).
- `LoadConfig` reads `app_id`, `app_slug`, `installation_id`, `owner_user_id`, `trigger_label` and the two base URLs with unknown keys refused (decision 0007).
- The checked-in file holds placeholders that fail `Validate` on purpose.
- Secrets come from the environment, set from the owner's secrets file outside the repository:
  - `CONCLAVE_GITHUB_WEBHOOK_SECRET`, which the server reads and then unsets;
  - `CONCLAVE_GITHUB_APP_KEY_FILE`, which only the worker reads.
- Base URLs must be https, or plain http to a loopback host for tests (`checkBaseURL`).

**A delivery arrives** (`webhook.go`: `Receiver.ServeHTTP`).
1. `checkShape`: only a JSON `POST` gets through (405, 415).
2. A slot from a semaphore of `MaxInFlight` (32). When none is free, the answer is 503.
3. `readBody`: at most `MaxBodyBytes` (1 MiB), otherwise 413.
4. `verified`: exactly one `X-Hub-Signature-256` header of the form `sha256=` plus 64 hex characters, compared with `hmac.Equal` against HMAC-SHA256 of the raw body. Anything else gets 401 with a fixed body. The SHA-1 header is never read.
5. `handle`: the delivery GUID is logged only if it is GUID-shaped (`deliveryID`), then the payload goes to `classify`.

**Who sent it** (`events.go`: `classify`; `provenance.go`: `Classify`).

Every event must come from a repository in `deploy/repos.yaml` and from the configured installation.

An `issues.labeled` event starts a run only when all of these hold:
- the label is the trigger label;
- the issue is open and is not a pull request;
- the sender classifies as `SourceOwner`.

The run starts from an `IssueSnapshot`, the issue as the signed payload carried it. Its `Author` is the issue's author, who may be a stranger if the owner labelled a stranger's issue.

An `issue_comment.created` event becomes a command only when all of these hold:
- the comment author classifies as the owner;
- the sender is the comment author and is a `User`;
- `ParseCommand` accepts the body.

`Classify` checks, in order:
1. The App flag (`performed_via_github_app`) equal to this App's ID gives `platform`.
2. Any other App flag, or type `Bot`, gives `bot`.
3. A `User` with the owner's ID gives `owner`.
4. Anything else gives `stranger`.

Everything ignored gets 202 and a log line with the reason and the sender's numeric ID, never the text. Reasons that need the owner's attention (`non_owner`, `bot`, `platform`, `sender_mismatch`, `malformed_command`) are logged at Info; the rest at Debug.

**The command grammar** (`command.go`: `ParseCommand`). Only the first line is read.
- It must match `/(approve|revise|ask|cancel|status|retry|help)` at column 0, lowercase, followed by whitespace or the end of the line.
- Arguments:
  - `/approve` takes at most one of `sequential` or `parallel`, plus `--accept-risk` at most once.
  - `/revise` and `/ask` need text, which is the rest of the line plus every following line.
  - The others take nothing, and nothing may follow them.
- Command words in backticks, quotes, code fences or indented text never sit at column 0. So the platform's own prose, which names commands in backticks (NFR-U2a), never parses as a command.

**Dispatch** (`dispatch.go`).
- `FeatureWorkflowID(repoID, issue)` is `feature/<numeric repository ID>/<issue>`. It is the same for every delivery about that issue, including after a rename.
- A command becomes a `CommandSignal` to the same ID, carrying the comment's ID so the workflow can drop a command it has already applied.
- The `Dispatcher` has a single consumer, so a label is dispatched before the `/approve` that follows it. Each starter call has a 10-second timeout and is never retried. A panic in a starter call is recovered and logged as `starter panicked`, without its value, so it costs one delivery rather than the server (`call`).
- `ErrAlreadyStarted` is logged as a duplicate and `ErrNoWorkflow` as `no_workflow`. Any other error is logged with the delivery ID, so the owner can redeliver it from GitHub.
- `enqueue` never blocks. When the queue is full, or `Close` has been called, the receiver answers 503.
- `Close(drain)` waits for the queue to empty. It then cancels Run's in-flight call and logs every job left behind by delivery ID.

**The server** (`cmd/server/main.go`).
- `run` wires `LoadConfig`, `llm.LoadRepoAccess` for the enabled set, the webhook secret, a JSON logger with `RedactAttr` as `ReplaceAttr`, the receiver and the dispatcher.
- Routes are `POST /webhooks/github` and `GET /healthz`.
- `newServer` bounds every phase: header 5s, read 10s, write 10s, idle 60s, headers 64 KiB.
- The server listens on 127.0.0.1:8080 by default, with the tunnel in front.
- On SIGINT or SIGTERM it calls `Shutdown`, then `Close(20s)`.
- `noEngine` stands in for the starter until ticket 8.

**Tokens** (`appauth.go`).
- `LoadAppKey` accepts only a regular file:
  - mode 0600 or 0400, and the same file that was inspected;
  - at most 64 KiB;
  - exactly one unencrypted PEM block holding an RSA key of at least 2048 bits, PKCS#1 or PKCS#8.

  The buffers are zeroed after reading.
- `appJWT` signs RS256 by hand: `iat` 60 seconds in the past, `exp` 9 minutes ahead, `iss` set to the App ID.
- `installationToken` sends `repository_ids: [repo.ID]` and the fixed `tokenPermissions`: contents, issues and pull_requests write, metadata read.
- It discards the reply unless:
  - every returned permission is within that set at or below the requested level;
  - neither `workflows` nor `administration` appears;
  - `repositories` is exactly that one repository;
  - the token has not already expired.
- Tokens are cached per repository until less than 10 minutes remain, and are trusted for at most an hour. A 401 on a repository call drops the cached token (`send`).

**REST calls** (`api.go`, `issues.go`, `pulls.go`).
- `do` sets GitHub's headers and the bearer token, and copies the anthropic adapter's pattern:
  - redirects are refused;
  - an error body is read up to 4 KiB and discarded;
  - transport causes are dropped;
  - every failure is an `*APIError`. Its text is the operation, the status and a request ID checked against a pattern, and it unwraps only to a sentinel.
- `IsPermanent` names what ticket 8's activities must not retry.
- Every write first checks the repository name with `checkFullName`, then the body (not empty, at most 65,536 characters). It also refuses a body carrying the live token or anything token-shaped (`refuseSecrets`).
- Every comment body, and every pull request title and body, passes through `sanitizeMarkdown` first (FR-SEC28). `@org/team` mentions are wrapped in a code span, so they notify nobody. HTML comments are removed, so nothing hidden reaches the thread. `<details>` sections stay, because the status comment uses them.
- `EditComment` reads the comment first and refuses one whose `performed_via_github_app` is not this App.
- `AddReaction` sends only six allowlisted reactions.
- `CreatePullRequest`:
  - requires an `agent/` head;
  - lists open pull requests from that head;
  - adopts one only if `<slug>[bot]` opened it against the same base, and refuses anyone else's;
  - otherwise creates the pull request, and after a 422 lists again (RL-2).

**Git** (`git.go`).
- `Push` validates before minting anything:
  - `CheckBranch`;
  - a full commit ID and a full-ID or empty lease;
  - the repository name;
  - an absolute tree path that `sandbox.VerifyTree` accepts.
- It then runs `git push --porcelain --atomic --no-verify --force-with-lease=refs/heads/<b>:<old> <url> <sha>:refs/heads/<b>`. An empty `<old>` means the branch must not exist yet.
- git's environment is built from scratch. `GIT_CONFIG_*` settings supply:
  - the Basic header scoped to that repository's URL;
  - `http.followRedirects=false`;
  - no hooks, no fsmonitor, no credential helper;
  - only the https protocol.
- A refusal becomes a `PushError` with a classified reason. The detail is redacted, including the base64 form of the credential, before it is truncated.
- `Clone` mints a token and calls ticket 3's `sandbox.Clone`. This ticket changed that function: it now sets `http.followRedirects=false`, and scopes the token's header to the clone URL's host, so submodules on the same host still authenticate.

**Quarantine** (`quarantine.go`).
- `NewQuarantine` draws a 128-bit nonce, one per model request.
- `Wrap(src, author, text)` turns invalid UTF-8 into U+FFFD. It then strips every case-insensitive occurrence of the nonce in one linear pass, which also catches occurrences that a removal splices together (`stripFold`).
- The result is:

  ```
  <<untrusted nonce=N source=<src> author_id=<id>>>
  text
  <</untrusted nonce=N>>
  ```
- `Admit` gives the rule ticket 8 applies:
  - owner text is always admitted;
  - platform text never is;
  - bot and stranger text is admitted, quarantined, only on private, sensitive and local-only repositories.

## Where to look

| File | What's in it |
|---|---|
| `internal/github/doc.go` | Which process holds which secret, and why |
| `internal/github/config.go` | `Config`, `LoadConfig`, `checkBaseURL`, `checkFullName`, the env variable names |
| `internal/github/secret.go` | `secret`, `redact`, `refuseSecrets`, `RedactAttr` |
| `internal/github/webhook.go` | `Receiver`: shape, slot, body cap, signature, logging |
| `internal/github/events.go` | Payload structs, `classify`, the ignore reasons |
| `internal/github/provenance.go` | `Source`, `Author`, `Classify` |
| `internal/github/command.go` | `ParseCommand` and its grammar |
| `internal/github/dispatch.go` | `WorkflowStarter`, `FeatureStart`, `CommandSignal`, `FeatureWorkflowID`, `Dispatcher` |
| `internal/github/quarantine.go` | `Quarantine`, `Wrap`, `Admit` |
| `internal/github/api.go` | `Client` interface, `App`, `NewApp`, `do`, `APIError`, `IsPermanent` |
| `internal/github/appauth.go` | `LoadAppKey`, `appJWT`, `installationToken` |
| `internal/github/issues.go`, `pulls.go` | Comments, reactions, pull requests |
| `internal/github/sanitize.go` | `sanitizeMarkdown`: team mentions and HTML comments |
| `internal/github/git.go` | `CheckBranch`, `Push`, `Clone`, `PushError` |
| `internal/sandbox/clone.go` | Ticket 3's clone, now refusing redirects with a host-scoped header |
| `internal/github/githubtest/` | Fake REST API, `FakeStarter`, signed deliveries, git server, runtime keys |
| `cmd/server/main.go` | Server wiring and shutdown |
| `deploy/github.yaml` | The owner's App configuration (placeholders) |

## What the tests prove

**Invalid signatures are rejected.**
- `TestWebhookRejectsBadSignature`: a missing header, SHA-1 only, a wrong prefix, non-hex, the wrong length, a flipped body byte, the wrong secret, or two headers each get 401, with no starter call and no secret in the log.
- `TestWebhookVerifiesBeforeParsing`: a bad signature on malformed JSON gets 401, not 400.
- `TestNewReceiverRejectsWeakSecret`.

**Only the owner commands.**
- `TestCommandFromNonOwnerIgnoredAndLogged`: the owner's login on another ID is ignored and logged as `non_owner` with its `sender_id`.
- `TestCommandInAppCommentTriggersNothing`: covers this App, another bot, the owner acting through an App, a forged "posted by owner" body, and a sender who is not the comment's author.
- `TestLabelByNonOwnerIgnored`, `TestLabelTrigger`.
- `TestClassifyProvenance`, `TestClassifyUnsetAppIDNeverPlatform`.

**Duplicates name the same workflow.**
- `TestDuplicateDeliveryMapsToSameWorkflowID` covers the same delivery twice, a new delivery ID, and a renamed repository. The second start is logged as a duplicate.
- `TestCommandSignalsFeatureWorkflowID`.

**Commands parse strictly.**
- `TestParseCommand`, about 30 cases, one behaviour each.
- `FuzzParseCommand`.
- `TestMalformedCommandNotDispatched`.

**Acknowledgement is fast and bounded.**
- `TestWebhookAcksBeforeDispatch`: a blocked starter still gets a 202 within two seconds.
- `TestWebhookQueueFullReturns503`, `TestWebhookConcurrencyLimit`, `TestWebhookBodyLimit`.
- `TestCommandsDispatchedInOrder`, `TestDispatcherDrainsOnClose`, `TestDispatcherDropsAfterDrainDeadline`, `TestEnqueueAfterCloseRefused`, `TestStarterErrorLoggedNotRetried`, `TestDispatcherDoesNotLeak`, `TestDispatcherSurvivesStarterPanic`.
- `TestNewServerBoundsEveryPhase`.

**A token is obtained, scoped and used.**
- `TestInstallationTokenObtainedAndUsed`: the fake checks the RS256 signature with the public key, the token request names one repository and four permissions, and the next call carries the token.
- `TestTokenWithExcessPermissionsRefused`: `workflows`, `administration`, an admin level, an extra repository, or a missing repository list each discard the token.
- `TestTokenScopedToOneRepository`, `TestTokenCachedUntilNearExpiry`, `TestUnauthorizedInvalidatesToken`.
- `TestPushAuthenticatesWithInstallationToken`, `TestCloneUsesInstallationToken`.

**No credential leaks.**
- `TestNoTokenInLogsOrErrors` runs deliveries through to App calls against a fake GitHub that echoes the credential into failure bodies, headers and request IDs. The log, written without `RedactAttr`, and every error hold no token, JWT, Basic credential or webhook secret.
- `TestAPIErrorsCarryNoSecrets`, `TestSecretHoldersFormatRedacted`, `TestSecretFormatsRedacted`, `TestRedact`, `TestRedactAttr`.
- `TestPushErrorRedacted`, `TestPushGitSeesNoInheritedEnv`, `TestCloneLeavesNoToken`.
- `TestOutgoingBodyCarryingTokenRefused`.
- `TestClientRefusesRedirect`, `TestPushDoesNotFollowRedirect`, and `TestCloneDoesNotFollowRedirect` in `internal/sandbox`.

**Posted text is sanitized.**
- `TestSanitizeMarkdown`: team mentions wrapped, HTML comments removed (including unterminated and spliced ones), user mentions, email addresses and `<details>` left alone.
- `TestOutgoingBodiesSanitized`: a created comment, an edited comment, and a pull request's title and body all reach GitHub sanitized.
- `TestLoadAppKey`.

**Writes land only where they should.**
- `TestCheckBranch`, `TestPushOnlyToAgentBranches`: refused branches never reach the server.
- `TestPushRefusesUnpreparedTree`, `TestPushRefusesNonSHACommit`.
- `TestPushNewBranchMustNotExist`, `TestPushLeaseRefusesStaleExpected`, `TestPushLeaseAllowsMatchingForce`, `TestPushRepeatedIsIdempotent`.
- `TestPushRemoteRejectionClassified`: GitHub's workflow-file refusal stays readable.
- `TestEditCommentRefusesCommentNotAuthoredByApp`, `TestEditCommentInPlace`: editing twice leaves one comment.
- `TestCreatePullRequestIdempotent`, `TestCreatePullRequestRace422`, `TestExistingPRByOtherAuthorNotAdopted`, `TestCreatePullRequestHeadMustBeAgentBranch`.
- `TestAddReactionAllowlist`, `TestRepoNameValidated`.

**Quarantine holds.**
- `TestQuarantineForgedDelimiters`: guessed closers, a leaked nonce, case variants, and spliced and doubly spliced nonces. A reference parser finds exactly one block with no nonce inside.
- `FuzzQuarantineWrap`, `TestQuarantineSpoofedAuthorMarkerStaysInside`.
- `TestAdmit`, `TestAdmitPublicTierExcludesNonOwner`.

## Left open

- **Ticket 8.**
  - Implement `WorkflowStarter` on Temporal with `WorkflowIDReusePolicy` REJECT_DUPLICATE and conflict policy FAIL. Map "already started" to `ErrAlreadyStarted`.
  - Drop repeated `CommandSignal`s by `CommentID`.
  - Load `LoadAppKey` and `NewApp` in the worker.
  - Build `SessionInput.IssueText` through `NewQuarantine`, `Wrap` and `Admit`.
  - Wrap `IsPermanent` errors as non-retryable.
  - Run `sandbox.PrepareTree` again after committing in a tree, because committing writes `.git/logs`, which `VerifyTree` refuses before `Push`.
- **Ticket 8b.**
  - Command audit and replies to malformed or refused commands.
  - The terminal-status check, by workflow ID.
  - Persisting the status comment's ID, so a retried create doesn't post twice.
  - `/help`.
  - Reactions acknowledging commands, sent from the worker, since the server has no key.
- **Ticket 9.**
  - `checks: read` added to `tokenPermissions`.
  - GraphQL `markPullRequestReadyForReview`.
  - Bot-identity commits.
  - The artifact-hash check before `Push`.
  - Branch cleanup.
- **Not in this ticket.**
  - Commands on pull request conversations and edited comments are ignored.
  - Per-sender rate limiting is left to ticket 10. Non-owners are dropped before the queue in the meantime.
  - A full queue answers 503 and relies on the owner redelivering, because GitHub does not retry on its own.
- **Residual risks.**
  - A third-party App acting with the owner's user-to-server token can apply the trigger label, because `labeled` payloads carry no App flag.
  - `RedactAttr` does not redact a non-error struct passed to `slog.Any`.
- **To confirm against real GitHub** before ticket 10's end-to-end run:
  - that the token reply lists `repositories` when `repository_ids` is sent;
  - that a string `iss` is accepted;
  - that comments made with an installation token carry `performed_via_github_app.id`.
- **Decision record:** [0010](../../decisions/0010-github-client-on-net-http.md) is proposed, for the owner to accept.

## Check your understanding

1. Why does the server never load the App's private key, and what does that cost ticket 8b?
2. A captured, validly signed `labeled` delivery is replayed a day later. What stops it from starting a second, paid run, and which part of that is ticket 8's job?
3. Why is a comment from the owner's own account still not a command when `performed_via_github_app` is set?
4. Name every place the installation token could leak in this package, and the control and test for each.
5. What stops a push from changing `.github/workflows` on the default branch? Name the client-side and GitHub-side controls.
6. Why does `Wrap` strip the nonce in a loop-free single pass rather than with one `strings.ReplaceAll`?
