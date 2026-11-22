-- The operator role, and the authenticated login on halt and command rows
-- (decision 0002).
--
-- Workers only ever read the kill switch, and only `conclave` writes it.
-- Until now both connected through conclave_app, so the worker's database
-- URL could also clear a halt by inserting a row directly, past the CLI's
-- terminal check and with an actor of 'cli' indistinguishable from the
-- owner's own resume. This migration separates the two:
--
--   - conclave_operator is the CLI's role. It is a member of conclave_app,
--     so it can do what the CLI's sweep and report need, and it alone may
--     insert into halt_events. Its LOGIN role is created at deployment
--     time, as for conclave_app, and its URL never reaches a worker.
--   - conclave_app keeps SELECT on halt_events and loses INSERT.
--
-- db_user records session_user, the login Postgres authenticated, on every
-- halt event and command, as state_transitions already does. A trigger
-- sets it, overwriting whatever the insert supplied, so the inserting
-- login cannot claim to be another one. Rows from before this migration
-- carry 'unrecorded'. What db_user cannot do is say which person or
-- process used a login, and the table owner can still rewrite anything:
-- the owner's credentials stay off every application process, which the
-- worker and the CLI check at startup (store.CurrentLogin).

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'conclave_operator') THEN
        CREATE ROLE conclave_operator NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT conclave_app TO conclave_operator;

REVOKE INSERT ON halt_events FROM conclave_app;
GRANT INSERT ON halt_events TO conclave_operator;

ALTER TABLE halt_events ADD COLUMN db_user text NOT NULL DEFAULT 'unrecorded' CHECK (db_user <> '');
ALTER TABLE halt_events ALTER COLUMN db_user DROP DEFAULT;
ALTER TABLE commands ADD COLUMN db_user text NOT NULL DEFAULT 'unrecorded' CHECK (db_user <> '');
ALTER TABLE commands ALTER COLUMN db_user DROP DEFAULT;

-- record_db_user stamps the authenticated login on an append-only row. The
-- search_path is pinned as for record_state_transition, though the body
-- names no table.
-- +goose StatementBegin
CREATE FUNCTION record_db_user() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    NEW.db_user := session_user;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER halt_events_db_user BEFORE INSERT ON halt_events
    FOR EACH ROW EXECUTE FUNCTION record_db_user();
CREATE TRIGGER commands_db_user BEFORE INSERT ON commands
    FOR EACH ROW EXECUTE FUNCTION record_db_user();

-- +goose Down

DROP TRIGGER commands_db_user ON commands;
DROP TRIGGER halt_events_db_user ON halt_events;
DROP FUNCTION record_db_user();
ALTER TABLE commands DROP COLUMN db_user;
ALTER TABLE halt_events DROP COLUMN db_user;
REVOKE INSERT ON halt_events FROM conclave_operator;
GRANT INSERT ON halt_events TO conclave_app;
REVOKE conclave_app FROM conclave_operator;
DROP ROLE IF EXISTS conclave_operator;
