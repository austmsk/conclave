# Ticket 8a — Budgets and attribution

**Status:** merged as PR #10 (`9d383a2`; feature `26e69b7`, review fixes `2fcbd22` and `d6921d2`) · **Spec:** [`milestone-1.md`](../../tickets/milestone-1.md#ticket-8a--budgets-and-attribution) · **Decision:** [0009](../../decisions/0009-budget-caps-route-to-needs-attention.md) · **Requirements:** FR-SEC9, NFR-M7 · **Review list:** RL-2

## In one paragraph

Every model call is now charged to a named feature request and, for attempt work, a named attempt, and it is checked against all three FR-SEC9 caps before any provider is reached. The caps are the attempt's own token budget, a token cap per feature request, and a USD cap per UTC calendar month across every repository. The check doesn't rely on what one process remembers. Before each call, one short Postgres transaction takes a global advisory lock, adds up settled spend and every live reservation, and either refuses or reserves the call's worst-case cost. After the reply, a second transaction records the cost event and deletes the reservation. There is no Temporal code here: this is `internal/llm` and `internal/store` only.

## Why it exists

After ticket 4, the budget was read from the ledger once and then tracked in memory. Two scopes for the same attempt could therefore each spend the whole budget. One example is an activity abandoned after a heartbeat timeout and its retry. A scope with no `AttemptID` had no cross-retry protection at all, and one with no `FeatureRequestID` wrote cost events that no feature-request cap could see. Per-feature-request and monthly caps didn't exist yet (`FR-SEC9`).

The ticket runs before ticket 5 because the agent runner has to tell an attempt budget hit (an attempt outcome, `token_limit`) from a platform cap hit (a `needs_attention` stop), and it needs the typed error to do that. `RL-2` shaped the write path: a retried settle must record one cost event, not two. Decision [0009](../../decisions/0009-budget-caps-route-to-needs-attention.md) records that a cap stops the feature request in `needs_attention`, and that the owner continues with `/retry`.

## How it works

**Attribution is required.** There are two constructors on `Router` (`internal/llm/scope.go`):

- `Router.Scope` is for attempt work. It requires `RepoID`, `FeatureRequestID`, `AttemptID`, `KeyPrefix`, and a positive `TokenBudget`.
- `Router.FeatureRequestScope` is for calls outside an attempt, such as analysis. It refuses an `AttemptID` or a budget, so attempt work can't be moved out from under its budget by picking the other constructor.

Both require every ID to be a canonical UUID and then call `CostLedger.Attributed`. That query checks that the feature request belongs to the repository and that the attempt belongs to the feature request, through its task and plan. A mismatch is `ErrInvalidScope`, returned before any call is made.

**Before each provider call: reserve** (`Scoped.reserve` → `store.CostLedger.Reserve`, in `internal/store/budget.go`):

1. Open a transaction at READ COMMITTED. Refuse to run at any other level, because a REPEATABLE READ snapshot taken before the lock would not see the previous holder's reservation.
2. Take `pg_advisory_xact_lock` on one global key (`budgetLockKey`). There is one key for every cap, so locks can't be taken in two different orders.
3. `readSpend`: a single statement sums `cost_events` plus unexpired `budget_reservations` for the attempt, the feature request, and the current UTC month. The month boundary is computed in SQL from `statement_timestamp()` and converted through UTC explicitly, so the session time zone can't move it.
4. Call the router's `decide` callback (`ask.decide` in `internal/llm/budget.go`). The token caps *shrink* the granted `max_tokens` when a call is near a cap, and refuse only when the input alone won't fit. The monthly cap is in dollars and refuses outright. Input is priced at the dearest of the input and cache rates, because the estimate can't tell how much the provider will bill as a cache write.
5. Insert the claim as a reservation, with `expires_at` set from the **database** clock: `ReservationTTL`, which is `MaxCallDuration` (10 min) plus 5 min.

A reservation is per provider call, priced at that model's rate, so each fallback in the chain reserves on its own.

**After the reply: settle or release** (`Scoped.settle` → `CostLedger.Settle` / `Release`, in `internal/store/costs.go`):

- If the provider billed something, insert the cost event and delete the reservation in one transaction. The event's idempotency key is `prefix/provider/replyID` with `ON CONFLICT DO NOTHING`, so a retried settle counts once (`RL-2`).
- If nothing was billed, the call only releases its reservation.
- Settling runs under `context.WithoutCancel`, with the later of the caller's deadline or 30 s. The provider has already been paid, so a cancelled caller must not lose the record.
- If settling fails, the call fails and the reservation stays until it expires, so the spend keeps counting in the meantime.

**Refusals.** Each one is a `BudgetError{Kind}` with kind `attempt`, `feature_request`, or `month`. It unwraps to the existing `ErrBudgetExceeded`, so `errors.Is` and `IsPermanent` hold and no new sentinel was added.

**Cap values.** These come from `budgets:` in `deploy/router.yaml` (`Budgets` in `internal/llm/config.go`). The shipped placeholder is `-1`, and validation fails on it, on a missing key, and on NaN. The Anthropic adapter's timeout is tied to `MaxCallDuration`, and `anthropic.New` refuses an injected client without a timeout inside that bound, so no call can outlive its reservation.

**Expired reservations are left in the table, not deleted.** A delete under the lock could wait on a `Settle` holding the same row and stall every model call. Skipping locked rows needs `UPDATE`, which `conclave_app` doesn't have. Expired rows are filtered out of every sum, and the `expires_at` index keeps those scans short.

## Where to look

| File | What's in it |
|---|---|
| `internal/llm/scope.go` | `Scope`, `Router.Scope`, `Router.FeatureRequestScope`, attribution check, `reserve`, `settle`, `settleContext` |
| `internal/llm/budget.go` | `BudgetKind`, `BudgetError`, `MaxCallDuration`, `ReservationTTL`, `Spend`, `Claim`, `ask.decide`, `fit` |
| `internal/llm/cost.go` | The `CostLedger` interface (`Attributed`, `Reserve`, `Settle`, `Release`) |
| `internal/llm/config.go` | `Budgets` and its validation |
| `internal/store/budget.go` | `Attributed`, `Reserve`, `Spend`, `lockBudgets`, `readSpend`, `insertReservation` |
| `internal/store/costs.go` | `Settle` and `Release` |
| `internal/store/migrations/00005_budget_reservations.sql` | `budget_reservations`, the `expires_at` and `cost_events (occurred_at)` indexes, and grants |
| `internal/llm/faketest/costs.go` | In-memory ledger with the same contract, for router tests |
| `deploy/router.yaml` | `budgets:` with failing placeholders |

## What the tests prove

| Guarantee | Test |
|---|---|
| A scope without attribution, or with an attempt outside its feature request, is refused | `TestScopeRequiresAttribution`, `TestAttributed` (`internal/llm/scope_test.go`, `internal/store/budget_test.go`) |
| A feature-request scope takes no attempt or budget but is still capped | `TestFeatureRequestScope`, `TestFeatureRequestScopeIsCapped` |
| Two live scopes for one attempt share one budget, including a call still in flight | `TestInFlightCallCountsAcrossScopes`, `TestConcurrentScopesStayInAttemptBudget`, `TestSecondScopeSeesInFlightCall` |
| Concurrent attempts can't pass the feature-request or monthly cap together | `TestConcurrentAttemptsStayInFeatureRequestCap`, `TestConcurrentCallsStayInMonthlyCap` |
| Each refusal names its kind, unwraps to `ErrBudgetExceeded`, and happens before a provider is reached | `TestCapRefusals` |
| The month boundary is UTC on both sides | `TestMonthIsUTC` |
| An expired reservation stops counting, on the database clock | `TestReservationExpiresOnDatabaseClock` |
| A retried settle records one event; settle is atomic | `TestSettleIsIdempotent`, `TestSettleIsAtomic` |
| A cancelled caller's billed reply is still recorded | `TestCancelledCallIsStillRecorded`, `TestSettleKeepsLongerDeadline` |
| `conclave_app` can't update a reservation | `TestAppRoleReservationGrants` |
| A reservation at the wrong isolation level is refused | `TestReserveRefusesOtherIsolation` |
| Reserving never waits on a settle | `TestReserveDoesNotWaitOnSettle` |
| Missing or placeholder caps fail startup | `TestLoadConfigRequiresBudgets`, `TestDeployedConfig` |

## Left open

- **What ticket 8 must know.** A `budget_cap` stop can come from a stale reservation left by a failed settle, not only from real spend; it clears when the reservation expires. Ticket 8 also maps `BudgetError` to the activity error type `budget_cap:<kind>`, because typed errors don't cross the activity boundary.
- **`store` imports `llm`** for `Spend` and `Claim`. This is a deliberate edge, to be explained in the decision record.
- **Settle can outlive its reservation.** Settle keeps the caller's longer deadline. Capping it would turn a short uncounted window into a permanent loss from the ledger. This trade-off belongs in the decision record.
- **Feature-request cap shrinks instead of refusing.** Near the cap, a call gets a smaller `max_tokens` rather than a refusal.
- **Refused calls use a rate-limiter slot.** The limiter is waited on before the reservation, so a refused call has already taken one.
- `Settle` and `Release` run at the server's default isolation. `Spend` has no production caller yet. There is no upper bound on prices. Each call pays for an isolation-check round trip.
- **Decision record owed** for the reservation design, written by the owner per [0001](../../decisions/0001-record-decisions.md).

## Check your understanding

1. An activity times out on heartbeat while its model call is still running, and Temporal starts a retry. Why can't the retry spend the attempt's budget a second time?
2. Why must the reservation transaction run at READ COMMITTED, and what would go wrong under REPEATABLE READ?
3. Why is `expires_at` set from the database clock rather than the worker's?
4. Why are expired reservations filtered out of the sums rather than deleted?
5. A call is near the feature-request cap. What does it get, and how does that differ from a call near the monthly cap?
6. Settling fails after the provider has billed. What happens to the spend, and when does it stop counting?
