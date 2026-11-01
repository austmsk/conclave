-- Pull request delivery (ticket 9, decision 0014).
--
-- One pull_requests row per delivered attempt, written ahead of every
-- outside action so a retried activity, or a /retry, resumes from what the
-- row records instead of acting again (RL-2). The row is created with the
-- pr_writer's output and the commit message built from it; the commit
-- message is part of every commit SHA the platform pushes, so it is never
-- rebuilt. Each later step fills its own columns once: the landed
-- integration commit, the base the work was rebased onto (FR-S18a records
-- it at branch creation), the rebased artifact's hash (FR-SEC27: a new
-- artifact, gated again), the pull request's head commit and identity,
-- and the moment it was marked ready. A trigger refuses any change to a
-- column once it holds a value, and the application role may update only
-- those columns.
--
-- CI results are not a column: CI can be re-run and the owner can /retry
-- after a failure, so each completed observation is a row of the
-- append-only pr_ci_results, and the latest row is the pull request's CI
-- result.

-- +goose Up

CREATE TABLE pull_requests (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    feature_request_id     uuid        NOT NULL REFERENCES feature_requests (id),
    task_id                uuid        NOT NULL REFERENCES tasks (id),
    -- One delivery per attempt: the attempt's artifact is what it delivers.
    attempt_id             uuid        NOT NULL UNIQUE REFERENCES attempts (id),
    integration_branch     text        NOT NULL CHECK (integration_branch ~ '^agent/[1-9][0-9]*/integration$'),
    pr_branch              text        NOT NULL CHECK (pr_branch ~ '^agent/[1-9][0-9]*/pr-[1-9][0-9]*$'),
    -- The base the attempt ran on (the plan's pinned base) and the
    -- artifact it produced there.
    original_base_sha      text        NOT NULL CHECK (original_base_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    original_artifact_hash text        NOT NULL CHECK (original_artifact_hash ~ '^[0-9a-f]{64}$'),
    -- The pr_writer's output, validated, and the commit message the
    -- platform built from it.
    title                  text        NOT NULL CHECK (title <> ''),
    summary                text        NOT NULL,
    risks                  text        NOT NULL,
    commit_message         text        NOT NULL CHECK (commit_message <> ''),
    -- Filled once each, in order.
    landed_commit          text        CHECK (landed_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    base_sha               text        CHECK (base_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    rebased_artifact_hash  text        CHECK (rebased_artifact_hash ~ '^[0-9a-f]{64}$'),
    head_commit            text        CHECK (head_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    number                 integer     CHECK (number > 0),
    node_id                text        CHECK (node_id <> ''),
    url                    text        CHECK (url <> ''),
    ready_at               timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    -- A pull request is recorded whole, and only after its head commit.
    CHECK ((number IS NULL) = (node_id IS NULL) AND (number IS NULL) = (url IS NULL)),
    CHECK (number IS NULL OR head_commit IS NOT NULL),
    CHECK (ready_at IS NULL OR number IS NOT NULL)
);

CREATE INDEX pull_requests_feature_request ON pull_requests (feature_request_id);

-- +goose StatementBegin
CREATE FUNCTION pull_requests_write_once() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF (OLD.landed_commit IS NOT NULL AND NEW.landed_commit IS DISTINCT FROM OLD.landed_commit)
       OR (OLD.base_sha IS NOT NULL AND NEW.base_sha IS DISTINCT FROM OLD.base_sha)
       OR (OLD.rebased_artifact_hash IS NOT NULL AND NEW.rebased_artifact_hash IS DISTINCT FROM OLD.rebased_artifact_hash)
       OR (OLD.head_commit IS NOT NULL AND NEW.head_commit IS DISTINCT FROM OLD.head_commit)
       OR (OLD.number IS NOT NULL AND NEW.number IS DISTINCT FROM OLD.number)
       OR (OLD.node_id IS NOT NULL AND NEW.node_id IS DISTINCT FROM OLD.node_id)
       OR (OLD.url IS NOT NULL AND NEW.url IS DISTINCT FROM OLD.url)
       OR (OLD.ready_at IS NOT NULL AND NEW.ready_at IS DISTINCT FROM OLD.ready_at) THEN
        RAISE EXCEPTION 'pull_requests outcome columns are write-once (pull request %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER pull_requests_write_once
    BEFORE UPDATE ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION pull_requests_write_once();

CREATE TABLE pr_ci_results (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id uuid        NOT NULL REFERENCES pull_requests (id),
    head_commit     text        NOT NULL CHECK (head_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    -- SHA-256 of the check runs observed (IDs and conclusions), so a
    -- retried read of the same runs is absorbed, and a re-run of CI, whose
    -- runs have new IDs, records a new row.
    observation     text        NOT NULL CHECK (observation ~ '^[0-9a-f]{64}$'),
    passed          boolean     NOT NULL,
    -- [{"name", "id", "conclusion"}], one per required check.
    checks          jsonb       NOT NULL,
    -- clock_timestamp, not now(): two observations in one transaction
    -- must still be ordered.
    observed_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (pull_request_id, observation)
);

CREATE INDEX pr_ci_results_latest ON pr_ci_results (pull_request_id, observed_at);

-- Granted explicitly: 00004 sets no default privileges.
GRANT SELECT, INSERT ON pull_requests TO conclave_app;
GRANT UPDATE (landed_commit, base_sha, rebased_artifact_hash, head_commit, number, node_id, url, ready_at)
    ON pull_requests TO conclave_app;
-- Append-only.
GRANT SELECT, INSERT ON pr_ci_results TO conclave_app;

-- +goose Down

REVOKE ALL PRIVILEGES ON pr_ci_results FROM conclave_app;
REVOKE ALL PRIVILEGES ON pull_requests FROM conclave_app;
DROP TABLE pr_ci_results;
DROP TRIGGER pull_requests_write_once ON pull_requests;
DROP FUNCTION pull_requests_write_once();
DROP TABLE pull_requests;
