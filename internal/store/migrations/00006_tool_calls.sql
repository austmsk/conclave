-- Tool-call audit: one row for every tool call an agent makes (FR-SEC24).
--
-- The worker writes the row before the call's result returns to the model,
-- so nothing the agent does afterwards can alter its own trail. The table is
-- append-only: the application role may read and insert, never update or
-- delete (threat T6), and a retried write of the same call is absorbed by
-- the unique key rather than doubled.

-- +goose Up

CREATE TABLE tool_calls (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id    uuid        NOT NULL REFERENCES attempts (id),
    -- Unique per execution of the agent loop. A retried activity runs a new
    -- session with its own key, so its steps never collide with the first
    -- run's; a retried write within one session hits the unique key below.
    session_key   text        NOT NULL CHECK (session_key <> ''),
    -- The session's tool calls numbered from 1 in the order they were
    -- answered, including calls refused or never executed.
    step          integer     NOT NULL CHECK (step > 0),
    call_id       text        NOT NULL,
    tool          text        NOT NULL,
    -- Exactly what the model sent, even when it is not JSON.
    arguments_raw bytea       NOT NULL,
    -- The same arguments as jsonb, for queries. Null when the bytes are not
    -- something Postgres accepts as jsonb; arguments_raw keeps them either
    -- way, so malformed arguments can never make the audit write fail.
    arguments     jsonb,
    exit_code     integer     NOT NULL,
    duration_ms   bigint      NOT NULL CHECK (duration_ms >= 0),
    -- Where the blob store keeps the full output, set whenever the model
    -- was shown a truncated result.
    artifact_key  text,
    -- A sanitized code naming what refused or failed, never raw provider or
    -- sandbox output, which may carry a credential.
    error         text,
    -- A path or argument refused on security grounds (FR-SEC29).
    rejected      boolean     NOT NULL,
    truncated     boolean     NOT NULL,
    -- Stamped by the worker when the call finished. No default: a row
    -- without the worker's time is a bug, not something to paper over.
    occurred_at   timestamptz NOT NULL,
    UNIQUE (attempt_id, session_key, step)
);

-- Gating counts an attempt's rejected calls (store.ToolCalls.RejectionCount).
CREATE INDEX tool_calls_rejected ON tool_calls (attempt_id) WHERE rejected;

-- Append-only, granted explicitly: 00004 sets no default privileges.
GRANT SELECT, INSERT ON tool_calls TO conclave_app;

-- +goose Down

REVOKE ALL PRIVILEGES ON tool_calls FROM conclave_app;
DROP TABLE tool_calls;
