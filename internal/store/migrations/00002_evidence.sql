-- Evidence: gate results and recorded decisions.

-- +goose Up

CREATE TABLE gate_results (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id    uuid        NOT NULL REFERENCES attempts (id),
    gate          text        NOT NULL CHECK (gate <> ''),
    passed        boolean     NOT NULL,
    -- Did not fail but needs human attention (execution-surface changes).
    flagged       boolean     NOT NULL DEFAULT false,
    -- Ties the result to the exact diff that was checked (FR-SEC27). Never
    -- empty: a gate result without a hash proves nothing.
    artifact_hash text        NOT NULL CHECK (artifact_hash <> ''),
    mode          text        NOT NULL CHECK (mode IN ('clean_room', 'reset_in_place')),
    detail        text        NOT NULL DEFAULT '',
    -- Full output in the blob store.
    artifact_key  text,
    ran_at        timestamptz NOT NULL DEFAULT now(),
    duration_ms   bigint      NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    -- Recording the same gate for the same artifact twice is a retried
    -- activity, not a second result (RL-2).
    UNIQUE (attempt_id, gate, artifact_hash)
);

CREATE TABLE decisions (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    feature_request_id uuid        NOT NULL REFERENCES feature_requests (id),
    -- Planning decisions carry a plan; implementation decisions carry an
    -- attempt. Either may be null, not both.
    plan_id            uuid        REFERENCES plans (id),
    attempt_id         uuid        REFERENCES attempts (id),
    subject            text        NOT NULL CHECK (subject <> ''),
    options_considered text[]      NOT NULL DEFAULT '{}',
    chosen             text        NOT NULL,
    rationale          text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (plan_id IS NOT NULL OR attempt_id IS NOT NULL)
);

CREATE INDEX decisions_feature_request_id ON decisions (feature_request_id);
CREATE INDEX decisions_attempt_id ON decisions (attempt_id);

-- +goose Down

DROP TABLE decisions;
DROP TABLE gate_results;
