-- 04 flat-team: audit/cost survive user deletion (PRD §15.4, Q4).
--
-- STARTUP-SAFETY: this migration runs while the server is starting. Keep the
-- transaction to metadata-only changes and fail quickly instead of waiting
-- behind application traffic with an AccessExclusiveLock queued. In
-- particular, DO NOT backfill actor_email here: production audit_log tables
-- can contain millions of rows, and rewriting them in this transaction holds
-- the DDL lock until the entire update commits.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
--
-- Flip audit_log.user_id from ON DELETE CASCADE to ON DELETE SET NULL,
-- make it nullable, and add a server-derived NOT-NULL actor_email so a
-- deleted user's history stays attributable. Drop the existing FK by
-- discovering its name (inline REFERENCES auto-names it) rather than
-- assuming the convention.
DO $$
DECLARE cname text;
BEGIN
    SELECT con.conname INTO cname
    FROM pg_constraint con
    JOIN pg_attribute att
      ON att.attrelid = con.conrelid AND att.attnum = ANY (con.conkey)
    WHERE con.conrelid = 'audit_log'::regclass
      AND con.contype = 'f'
      AND att.attname = 'user_id';
    IF cname IS NOT NULL THEN
        EXECUTE format('ALTER TABLE audit_log DROP CONSTRAINT %I', cname);
    END IF;
END $$;

ALTER TABLE audit_log ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;

-- On Postgres 11+, adding a column with a constant default is metadata-only.
-- Existing rows intentionally retain ''. New writes always supply the
-- server-derived email. Historical rows are filled by the separate resumable
-- backfill, outside the startup migration transaction.
ALTER TABLE audit_log ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_request_cost ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';
