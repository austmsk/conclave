-- Gate-to-push integrity (FR-SEC27).
--
-- The push path trusts gate_results.passed and attempts.artifact_hash, so
-- neither may be rewritten once recorded. 00004 granted the application role
-- UPDATE on gate_results; a result is evidence about one artifact, and a
-- retried gate run inserts with ON CONFLICT DO NOTHING rather than updating,
-- so UPDATE is withdrawn. An attempt's artifact hash is set once, when the
-- diff is frozen; the trigger refuses any later change, so a passing result
-- for one artifact cannot be pointed at another.

-- +goose Up

REVOKE UPDATE ON gate_results FROM conclave_app;

-- The hash is gate.HashArtifact's output: lowercase hex SHA-256. Anything
-- else is a bug in the caller, not a hash.
ALTER TABLE gate_results
    ADD CONSTRAINT gate_results_artifact_hash_sha256
    CHECK (artifact_hash ~ '^[0-9a-f]{64}$');

-- +goose StatementBegin
CREATE FUNCTION attempts_artifact_hash_write_once() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF OLD.artifact_hash IS NOT NULL
       AND NEW.artifact_hash IS DISTINCT FROM OLD.artifact_hash THEN
        RAISE EXCEPTION 'attempts.artifact_hash is write-once (attempt %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER attempts_artifact_hash_write_once
    BEFORE UPDATE OF artifact_hash ON attempts
    FOR EACH ROW EXECUTE FUNCTION attempts_artifact_hash_write_once();

-- +goose Down

DROP TRIGGER attempts_artifact_hash_write_once ON attempts;
DROP FUNCTION attempts_artifact_hash_write_once();
ALTER TABLE gate_results DROP CONSTRAINT gate_results_artifact_hash_sha256;
GRANT UPDATE ON gate_results TO conclave_app;
