-- Work records: repos -> feature_requests -> plans -> tasks -> attempts.
--
-- Conventions used throughout the schema:
--   * Primary keys are uuids. Callers may supply the id so a retried insert is
--     a no-op with ON CONFLICT DO NOTHING (RL-2).
--   * Lifecycle columns are named `state` on every table so the store's
--     compare-and-set helper runs the same UPDATE ... WHERE state = expected
--     against each of them (RL-13). Values are constrained by CHECK rather
--     than an enum type so a later milestone can add a value with a plain
--     ALTER; legality of moves between values lives in the Go transition
--     table, not here.
--   * updated_at is maintained by application code, not triggers.

-- +goose Up

CREATE TABLE repos (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name      text        NOT NULL UNIQUE,
    -- No tier, no run (FR-SEC12). NOT NULL means a repo row cannot exist
    -- without one.
    tier           text        NOT NULL CHECK (tier IN ('public', 'private', 'sensitive', 'local_only')),
    default_branch text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE feature_requests (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id      uuid        NOT NULL REFERENCES repos (id),
    issue_number integer     NOT NULL CHECK (issue_number > 0),
    title        text        NOT NULL,
    state        text        NOT NULL CHECK (state IN (
                     'planning', 'awaiting_approval', 'executing',
                     'needs_attention', 'delivered', 'cancelled')),
    -- Set when state is needs_attention; the owner reads it to decide.
    reason       text,
    -- Temporal workflow id, derived from the issue number, so a run can be
    -- followed from this row into the Temporal UI (RL-14).
    workflow_id  text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- One record per issue. A duplicate webhook that tries to create a
    -- second one conflicts here.
    UNIQUE (repo_id, issue_number)
);

CREATE TABLE plans (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    feature_request_id uuid        NOT NULL REFERENCES feature_requests (id),
    version            integer     NOT NULL CHECK (version > 0),
    state              text        NOT NULL CHECK (state IN ('proposed', 'approved', 'superseded', 'rejected')),
    -- Pinned at approval (FR-S19); an approved plan must carry one.
    base_sha           text,
    -- The plan document: task specs and risk flags, as JSON.
    body               jsonb       NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    approved_at        timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (feature_request_id, version),
    CHECK (state <> 'approved' OR (base_sha IS NOT NULL AND approved_at IS NOT NULL))
);

-- At most one approved plan per feature request (RL-4). A plain unique index
-- on feature_request_id would allow only one plan of any state; the WHERE
-- clause restricts uniqueness to approved rows, so proposed, superseded and
-- rejected versions can accumulate freely.
CREATE UNIQUE INDEX plans_one_approved_per_feature_request
    ON plans (feature_request_id)
    WHERE state = 'approved';

CREATE TABLE tasks (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id    uuid        NOT NULL REFERENCES plans (id),
    -- TaskSpec.Key, stable within a plan (for example "T2-endpoint").
    key        text        NOT NULL CHECK (key <> ''),
    -- contracts.TaskSpec as JSON.
    spec       jsonb       NOT NULL,
    -- Milestone 1 statuses only; later milestones extend the CHECK.
    state      text        NOT NULL CHECK (state IN ('running', 'done', 'needs_attention', 'cancelled')),
    reason     text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (plan_id, key)
);

CREATE TABLE attempts (
    id                  uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id             uuid           NOT NULL REFERENCES tasks (id),
    ordinal             integer        NOT NULL CHECK (ordinal > 0),
    state               text           NOT NULL CHECK (state IN ('running', 'passed', 'failed', 'errored')),
    reason              text,
    -- Provenance is columns rather than JSON because Milestone 2 groups
    -- results by model and prompt version.
    role                text           NOT NULL,
    prompt_version      text           NOT NULL,
    model               text           NOT NULL,
    provider            text           NOT NULL,
    input_tokens        bigint         NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens       bigint         NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_usd            numeric(12, 6) NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    steps               integer        NOT NULL DEFAULT 0 CHECK (steps >= 0),
    -- Hash of the frozen diff; the push path refuses anything else (FR-SEC27).
    artifact_hash       text,
    -- contracts.CompletionReport as JSON.
    report              jsonb,
    sandbox_id          text,
    started_at          timestamptz    NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    -- Tracked separately from state because cleanup must happen however the
    -- attempt ended, including cancellation (RL-3).
    sandbox_released_at timestamptz,
    updated_at          timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (task_id, ordinal)
);

-- The orphan reaper's query: attempts whose sandbox has not been released.
CREATE INDEX attempts_sandbox_unreleased
    ON attempts (started_at)
    WHERE sandbox_released_at IS NULL;

CREATE INDEX feature_requests_repo_id ON feature_requests (repo_id);
CREATE INDEX plans_feature_request_id ON plans (feature_request_id);
CREATE INDEX tasks_plan_id ON tasks (plan_id);
CREATE INDEX attempts_task_id ON attempts (task_id);

-- +goose Down

DROP TABLE attempts;
DROP TABLE tasks;
DROP TABLE plans;
DROP TABLE feature_requests;
DROP TABLE repos;
