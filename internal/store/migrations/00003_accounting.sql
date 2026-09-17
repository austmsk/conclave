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
    -- by FR-SEC10; an anonymous transition is a bug. Self-reported by the
    -- application, so it names the cause, not the identity.
    actor       text        NOT NULL CHECK (actor <> ''),
    -- The authenticated database login that made the change, taken from
    -- session_user inside the trigger. Unlike actor it cannot be forged by
    -- the application, and unlike current_user it survives SET ROLE.
    db_user     text        NOT NULL CHECK (db_user <> ''),
    -- The record's revision after this change: 1 for its first transition.
    revision    integer     NOT NULL CHECK (revision > 0),
    reason      text,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX state_transitions_entity ON state_transitions (entity_kind, entity_id, occurred_at);

-- record_state_transition makes the audit log structural rather than a
-- convention (FR-SEC10). It fires on any change to a state column, writes
-- the audit row itself, bumps the record's revision, and refuses the change
-- unless the transaction has named an actor through
-- set_config('conclave.actor', ..., true). No database role short of the
-- table owner can move a record without leaving a row here: the function's
-- search_path is pinned with pg_temp last and the target is
-- schema-qualified, so a temporary table named state_transitions cannot
-- shadow the real one (00004 also withholds TEMP from the application
-- role), and the application role can neither disable the trigger nor
-- change session_replication_role.
--
-- What the trigger cannot do is authenticate the actor string; that is
-- whatever the caller set. db_user records the login Postgres itself
-- authenticated. Legality of the move is not checked here; that lives in
-- the Go transition table, which is the single source of truth.
--
-- SQLSTATE CO001 is Conclave's own code for an unattributed state change.
-- +goose StatementBegin
CREATE FUNCTION record_state_transition() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    actor text := current_setting('conclave.actor', true);
    kind  text;
BEGIN
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NEW;
    END IF;
    IF actor IS NULL OR actor = '' THEN
        RAISE EXCEPTION 'state change on % without an actor: use store.Transition', TG_TABLE_NAME
            USING ERRCODE = 'CO001';
    END IF;
    kind := CASE TG_TABLE_NAME
        WHEN 'feature_requests' THEN 'feature_request'
        WHEN 'plans'            THEN 'plan'
        WHEN 'tasks'            THEN 'task'
        WHEN 'attempts'         THEN 'attempt'
    END;
    NEW.revision := OLD.revision + 1;
    INSERT INTO public.state_transitions
        (entity_kind, entity_id, from_state, to_state, actor, db_user, revision, reason)
    VALUES (kind, NEW.id, OLD.state, NEW.state, actor, session_user, NEW.revision, NEW.reason);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER feature_requests_state_transition
    BEFORE UPDATE OF state ON feature_requests
    FOR EACH ROW EXECUTE FUNCTION record_state_transition();
CREATE TRIGGER plans_state_transition
    BEFORE UPDATE OF state ON plans
    FOR EACH ROW EXECUTE FUNCTION record_state_transition();
CREATE TRIGGER tasks_state_transition
    BEFORE UPDATE OF state ON tasks
    FOR EACH ROW EXECUTE FUNCTION record_state_transition();
CREATE TRIGGER attempts_state_transition
    BEFORE UPDATE OF state ON attempts
    FOR EACH ROW EXECUTE FUNCTION record_state_transition();

-- +goose Down

DROP TRIGGER attempts_state_transition ON attempts;
DROP TRIGGER tasks_state_transition ON tasks;
DROP TRIGGER plans_state_transition ON plans;
DROP TRIGGER feature_requests_state_transition ON feature_requests;
DROP FUNCTION record_state_transition();
DROP TABLE state_transitions;
DROP TABLE cost_events;
