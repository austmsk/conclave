-- Accounting: cost events and the state-transition audit log.
--
-- Both tables are append-only. Rows are never updated or deleted by the
-- application, and 00004_app_role.sql withholds those privileges from the
-- application role so a compromised worker cannot rewrite history (T6 in the
-- threat model, FR-SEC10).

-- +goose Up

CREATE TABLE cost_events (
    id                 uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Chosen by the caller before the model call is made. A retried activity
    -- inserts the same key and conflicts, so cost is never counted twice
    -- (RL-2). ON CONFLICT DO NOTHING needs only INSERT privilege.
    idempotency_key    text           NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    repo_id            uuid           NOT NULL REFERENCES repos (id),
    feature_request_id uuid           REFERENCES feature_requests (id),
    attempt_id         uuid           REFERENCES attempts (id),
    role               text           NOT NULL,
    provider           text           NOT NULL,
    model              text           NOT NULL,
    input_tokens       bigint         NOT NULL CHECK (input_tokens >= 0),
    output_tokens      bigint         NOT NULL CHECK (output_tokens >= 0),
    cost_usd           numeric(12, 6) NOT NULL CHECK (cost_usd >= 0),
    occurred_at        timestamptz    NOT NULL DEFAULT now()
);

-- Per-repository and per-month caps (FR-SEC9) sum over this.
CREATE INDEX cost_events_repo_occurred ON cost_events (repo_id, occurred_at);
CREATE INDEX cost_events_feature_request_id ON cost_events (feature_request_id);
CREATE INDEX cost_events_attempt_id ON cost_events (attempt_id);

CREATE TABLE state_transitions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_kind text        NOT NULL CHECK (entity_kind IN ('feature_request', 'plan', 'task', 'attempt')),
    entity_id   uuid        NOT NULL,
    from_state  text        NOT NULL,
    to_state    text        NOT NULL,
    -- Who caused it: an owner command, a workflow, or a system job. Required
    -- by FR-SEC10; an anonymous transition is a bug.
    actor       text        NOT NULL CHECK (actor <> ''),
    reason      text,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX state_transitions_entity ON state_transitions (entity_kind, entity_id, occurred_at);

-- +goose Down

DROP TABLE state_transitions;
DROP TABLE cost_events;
