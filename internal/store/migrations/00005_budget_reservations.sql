-- Budget reservations: spend claimed by model calls still in flight.
--
-- The caps of FR-SEC9 (per attempt, per feature request, per UTC month) are
-- checked before each model call against settled spend in cost_events plus
-- the live rows here. A call claims a reservation inside one short
-- transaction holding a global advisory lock, and its settlement inserts the
-- cost event and deletes the reservation in one transaction, so the spend
-- is never counted in neither place and is in both only until that commit.
--
-- Reservations cannot live in cost_events: that table is append-only and
-- its CHECKs forbid the negative rows that cancelling a claim would need.
--
-- expires_at is set from the database clock, never the worker's. A process
-- that dies mid-call leaves its reservation behind; it keeps counting until
-- it expires, which covers a call the dead process may still have in flight
-- at the provider, and then stops counting so the budget is not held
-- forever. The next reservation after that deletes it.

-- +goose Up

CREATE TABLE budget_reservations (
    id                 uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Null for a call made outside any attempt, such as analysis. Those
    -- still count against the feature request and the month.
    attempt_id         uuid           REFERENCES attempts (id),
    feature_request_id uuid           NOT NULL REFERENCES feature_requests (id),
    tokens             bigint         NOT NULL CHECK (tokens >= 0),
    cost_usd           numeric(12, 6) NOT NULL CHECK (cost_usd >= 0),
    expires_at         timestamptz    NOT NULL
);

-- Every reservation deletes the expired rows and sums the live ones, while
-- holding the global budget lock, so both are range scans on expiry.
CREATE INDEX budget_reservations_expires ON budget_reservations (expires_at);

-- The monthly cap sums cost_usd across every repository. The existing
-- (repo_id, occurred_at) index leads with the repository and cannot serve
-- that range scan.
CREATE INDEX cost_events_occurred ON cost_events (occurred_at);

-- Rows are claimed and released, never changed, so UPDATE is not granted.
-- DELETE is, because releasing is the table's purpose. That means a
-- compromised worker can release reservations: they coordinate honest
-- workers, and are not an audit record. Settled spend stays in cost_events,
-- which the application role cannot change (threat T6).
GRANT SELECT, INSERT, DELETE ON budget_reservations TO conclave_app;

-- +goose Down

REVOKE ALL PRIVILEGES ON budget_reservations FROM conclave_app;
DROP INDEX cost_events_occurred;
DROP TABLE budget_reservations;
