-- The application database role.
--
-- Migrations run as the database owner. The server and workers connect as a
-- LOGIN role that is a member of conclave_app (created at deployment time,
-- outside migrations, because credentials never live in this repository) or
-- SET ROLE conclave_app after connecting. Either way the privileges below are
-- the ceiling of what application code can do.
--
-- Ordinary tables: read, insert, update. Nothing in the design deletes a
-- domain record, so DELETE is not granted.
-- Append-only tables: read and insert only (FR-SEC10, threat T6).
--
-- No ALTER DEFAULT PRIVILEGES: a future migration that adds a table grants
-- exactly what that table needs, so an append-only table cannot inherit
-- UPDATE by accident.

-- +goose Up

-- Roles are cluster-wide, so a second database in the same cluster may have
-- created it already.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'conclave_app') THEN
        CREATE ROLE conclave_app NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO conclave_app;

GRANT SELECT, INSERT, UPDATE
    ON repos, feature_requests, plans, tasks, attempts, gate_results, decisions
    TO conclave_app;

GRANT SELECT, INSERT
    ON cost_events, state_transitions
    TO conclave_app;

-- Temporary tables are searched before public, so a temp table named after
-- the audit table is the one way a non-owner could try to divert audit
-- rows. The trigger pins its search_path against this; withholding TEMP
-- from everyone but the owner is the second layer. PUBLIC holds TEMP on
-- every database by default, hence the revoke rather than a missing grant.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('REVOKE TEMP ON DATABASE %I FROM PUBLIC', current_database());
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT TEMP ON DATABASE %I TO PUBLIC', current_database());
END
$$;
-- +goose StatementEnd
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM conclave_app;
REVOKE USAGE ON SCHEMA public FROM conclave_app;
DROP ROLE IF EXISTS conclave_app;
