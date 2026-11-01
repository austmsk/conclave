-- The kill switch (ticket 10a, FR-SEC11, decision 0015).
--
-- `conclave halt` records a halt here before it cancels anything, and
-- `conclave resume` records the clear. Workers poll the latest event: while
-- it is a set they stop, after a grace period that lets cancellations
-- finish, and refuse to start. The halt lives in Postgres rather than in
-- Temporal or on GitHub so that it holds when either of those is the
-- problem (threat S3), and so the webhook server, which has no database
-- access, is not given any.
--
-- The table is append-only: the application role may read and insert,
-- never update or delete, so a compromised worker cannot erase a halt or
-- the record that one happened (threat T6). The current state is the event
-- with the highest id; id rather than occurred_at orders events, because
-- two events can share a timestamp.

-- +goose Up

CREATE TABLE halt_events (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        text        NOT NULL CHECK (kind IN ('set', 'clear')),
    -- Why the owner halted or resumed, in their words. Never a secret:
    -- the CLI passes it through the same redaction as alert text.
    reason      text        NOT NULL CHECK (reason <> ''),
    actor       text        NOT NULL CHECK (actor <> ''),
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Append-only, granted explicitly: 00004 sets no default privileges.
GRANT SELECT, INSERT ON halt_events TO conclave_app;

-- +goose Down

REVOKE ALL PRIVILEGES ON halt_events FROM conclave_app;
DROP TABLE halt_events;
