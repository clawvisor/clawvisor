\set ON_ERROR_STOP on

-- Run with psql AFTER migration 062 has committed and the new application
-- revision is healthy. Do not wrap this script in BEGIN/COMMIT: the procedure
-- commits every batch so it never creates another multi-million-row
-- transaction. The script is resumable; rows already carrying actor_email are
-- skipped on subsequent runs.
--
-- Override the default batch size with:
--   psql ... -v batch_size=5000 -f scripts/postgres/post-062-backfill.sql
--
-- psql variables are plain strings, so the override is quoted and cast to
-- integer at the call site below. That turns a non-numeric value into an
-- immediate "invalid input syntax for type integer" instead of the confusing
-- "column ... does not exist" that an unquoted interpolation would produce.
-- The numeric range is checked inside the procedure.
\if :{?batch_size}
\else
\set batch_size 10000
\endif

CREATE OR REPLACE PROCEDURE clawvisor_backfill_actor_email(batch_size integer)
LANGUAGE plpgsql
AS $$
DECLARE
    -- Reject a batch size outside this range rather than clamping it. A batch
    -- of zero or less makes every LIMIT return no rows, so both loops would
    -- exit on their first iteration and the script would report success having
    -- written nothing at all. An unbounded batch recreates the single giant
    -- transaction this script exists to avoid. In either case the operator
    -- asked for something the script cannot honour, so say so loudly.
    min_batch_size constant integer := 1;
    max_batch_size constant integer := 100000;
    -- SKIP LOCKED lets a batch step over rows held by concurrent writers so
    -- the backfill never queues behind live traffic, but the keyset cursor
    -- advances past those rows all the same, so a single forward pass can
    -- leave them behind. Repeat whole passes from the start of the table until
    -- one finds nothing left blank. The bound stops a row that is locked
    -- indefinitely from spinning here forever; if we hit it we warn with the
    -- outstanding count instead of pretending the backfill finished.
    max_passes constant integer := 10;
    changed integer;
    last_id text := '';
    batch_last_id text;
    pass integer;
    remaining bigint;
BEGIN
    IF batch_size IS NULL OR batch_size < min_batch_size OR batch_size > max_batch_size THEN
        RAISE EXCEPTION 'batch_size must be an integer between % and %, got %',
            min_batch_size, max_batch_size, COALESCE(batch_size::text, 'NULL')
            USING HINT = 'Re-run with, for example, -v batch_size=10000.';
    END IF;

    pass := 0;
    LOOP
        pass := pass + 1;
        last_id := '';
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

        -- The forward pass is done, so anything still blank was either skipped
        -- while locked or written by a concurrent transaction that started
        -- before the new revision was fully rolled out. This count is the only
        -- honest completion test; it costs one scan per pass.
        SELECT count(*) INTO remaining FROM audit_log WHERE actor_email = '';
        COMMIT;

        EXIT WHEN remaining = 0;

        IF pass >= max_passes THEN
            RAISE WARNING 'audit_log: % row(s) still have an empty actor_email after % passes, most likely held by a long-running transaction. Re-run this script once it has finished.',
                remaining, max_passes;
            EXIT;
        END IF;

        RAISE NOTICE 'audit_log: % row(s) were skipped while locked; starting pass % of %.',
            remaining, pass + 1, max_passes;
        -- Back off before rescanning so the transactions holding those rows
        -- get a chance to finish. Commit first: sleeping inside an open
        -- transaction would pin the snapshot and hold back vacuum.
        PERFORM pg_sleep(least(pass, 5));
        COMMIT;
    END LOOP;

    -- llm_request_cost is keyed by audit_id and is smaller, but it is written
    -- by the same live request path, so it needs the identical convergence
    -- treatment.
    pass := 0;
    LOOP
        pass := pass + 1;
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

        SELECT count(*) INTO remaining FROM llm_request_cost WHERE actor_email = '';
        COMMIT;

        EXIT WHEN remaining = 0;

        IF pass >= max_passes THEN
            RAISE WARNING 'llm_request_cost: % row(s) still have an empty actor_email after % passes, most likely held by a long-running transaction. Re-run this script once it has finished.',
                remaining, max_passes;
            EXIT;
        END IF;

        RAISE NOTICE 'llm_request_cost: % row(s) were skipped while locked; starting pass % of %.',
            remaining, pass + 1, max_passes;
        PERFORM pg_sleep(least(pass, 5));
        COMMIT;
    END LOOP;
END
$$;

CALL clawvisor_backfill_actor_email(:'batch_size'::integer);
DROP PROCEDURE clawvisor_backfill_actor_email(integer);

-- These indexes support OSS-only fleet-wide admin reads. CONCURRENTLY keeps
-- audit ingestion available while Postgres scans the large audit table. Each
-- statement runs at the top level in psql autocommit mode, as CREATE INDEX
-- CONCURRENTLY and DROP INDEX CONCURRENTLY both require; neither may move into
-- the procedure or a DO block, because those execute inside a transaction.
--
-- A concurrent build that fails or is interrupted leaves the index behind
-- flagged invalid. The planner never uses an invalid index, and a rerun of
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS sees the relation already exists and
-- skips it, so without this guard the index would stay dead through every
-- future run of the script. Look for that corpse and drop it first.
SELECT COALESCE(
    (SELECT NOT i.indisvalid
     FROM pg_index i
     WHERE i.indexrelid = to_regclass('idx_audit_time')),
    false) AS rebuild_idx_audit_time
\gset

\if :rebuild_idx_audit_time
\echo 'idx_audit_time exists but is INVALID (an earlier concurrent build did not finish); dropping it before rebuilding.'
DROP INDEX CONCURRENTLY IF EXISTS idx_audit_time;
\endif

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_audit_time
    ON audit_log(timestamp DESC);

SELECT COALESCE(
    (SELECT NOT i.indisvalid
     FROM pg_index i
     WHERE i.indexrelid = to_regclass('idx_llm_cost_time')),
    false) AS rebuild_idx_llm_cost_time
\gset

\if :rebuild_idx_llm_cost_time
\echo 'idx_llm_cost_time exists but is INVALID (an earlier concurrent build did not finish); dropping it before rebuilding.'
DROP INDEX CONCURRENTLY IF EXISTS idx_llm_cost_time;
\endif

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_llm_cost_time
    ON llm_request_cost(timestamp);

-- The replacement FK was installed NOT VALID so startup did not scan the
-- entire audit table while holding its DDL transaction. Validation permits
-- normal reads and writes while it checks historical rows.
ALTER TABLE audit_log VALIDATE CONSTRAINT audit_log_user_id_fkey;

-- Final gate: make the exit status tell the truth about completeness.
--
-- The convergence loops above warn when they give up on rows pinned by a
-- long-running transaction, but a WARNING leaves psql's exit status at 0, so
-- anything driving this script from cron or CI would record a clean success
-- over an unfinished backfill — the same "looks done, isn't" failure the
-- SKIP LOCKED cursor had. Fail here instead.
--
-- Deliberately placed last. Everything above is already committed (psql is in
-- autocommit), so the batches that did land, both concurrent index builds, and
-- the constraint validation all persist. Re-running is safe and picks up only
-- what is still outstanding.
DO $$
DECLARE
    audit_blank bigint;
    cost_blank  bigint;
BEGIN
    SELECT count(*) INTO audit_blank FROM audit_log WHERE actor_email = '';
    SELECT count(*) INTO cost_blank FROM llm_request_cost WHERE actor_email = '';
    IF audit_blank > 0 OR cost_blank > 0 THEN
        RAISE EXCEPTION 'backfill incomplete: % audit_log and % llm_request_cost row(s) still have an empty actor_email',
            audit_blank, cost_blank
            USING HINT = 'Rows were most likely pinned by a long-running transaction. Indexes and constraint validation are already in place; re-run this script once that transaction has finished.';
    END IF;
END
$$;
