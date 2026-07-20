\set ON_ERROR_STOP on

-- Run with psql AFTER migration 062 has committed and the new application
-- revision is healthy. Do not wrap this script in BEGIN/COMMIT: the procedure
-- commits every batch so it never creates another multi-million-row
-- transaction. The script is resumable; rows already carrying actor_email are
-- skipped on subsequent runs.
--
-- Override the default batch size with:
--   psql ... -v batch_size=5000 -f scripts/postgres/post-062-backfill.sql
\if :{?batch_size}
\else
\set batch_size 10000
\endif

CREATE OR REPLACE PROCEDURE clawvisor_backfill_actor_email(batch_size integer)
LANGUAGE plpgsql
AS $$
DECLARE
    changed integer;
    last_id text := '';
    batch_last_id text;
BEGIN
    LOOP
        WITH batch AS MATERIALIZED (
            SELECT a.id, COALESCE(u.email, '(deleted-user)') AS email
            FROM audit_log a
            LEFT JOIN users u ON u.id = a.user_id
            WHERE a.actor_email = '' AND a.id > last_id
            ORDER BY a.id
            LIMIT batch_size
            FOR UPDATE OF a SKIP LOCKED
        ), updated AS (
            UPDATE audit_log a
            SET actor_email = batch.email
            FROM batch
            WHERE a.id = batch.id
            RETURNING a.id
        )
        SELECT COUNT(*), MAX(id) INTO changed, batch_last_id FROM updated;

        EXIT WHEN changed = 0;
        last_id := batch_last_id;
        COMMIT;
    END LOOP;

    last_id := '';
    LOOP
        WITH batch AS MATERIALIZED (
            SELECT c.audit_id, COALESCE(u.email, '(deleted-user)') AS email
            FROM llm_request_cost c
            LEFT JOIN users u ON u.id = c.user_id
            WHERE c.actor_email = '' AND c.audit_id > last_id
            ORDER BY c.audit_id
            LIMIT batch_size
            FOR UPDATE OF c SKIP LOCKED
        ), updated AS (
            UPDATE llm_request_cost c
            SET actor_email = batch.email
            FROM batch
            WHERE c.audit_id = batch.audit_id
            RETURNING c.audit_id
        )
        SELECT COUNT(*), MAX(audit_id) INTO changed, batch_last_id FROM updated;

        EXIT WHEN changed = 0;
        last_id := batch_last_id;
        COMMIT;
    END LOOP;
END
$$;

CALL clawvisor_backfill_actor_email(:batch_size);
DROP PROCEDURE clawvisor_backfill_actor_email(integer);

-- These indexes support OSS-only fleet-wide admin reads. CONCURRENTLY keeps
-- audit ingestion available while Postgres scans the large audit table. Each
-- statement runs in psql autocommit mode as CREATE INDEX CONCURRENTLY requires.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_time
    ON audit_log(timestamp DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_llm_cost_time
    ON llm_request_cost(timestamp);

-- The replacement FK was installed NOT VALID so startup did not scan the
-- entire audit table while holding its DDL transaction. Validation permits
-- normal reads and writes while it checks historical rows.
ALTER TABLE audit_log VALIDATE CONSTRAINT audit_log_user_id_fkey;
