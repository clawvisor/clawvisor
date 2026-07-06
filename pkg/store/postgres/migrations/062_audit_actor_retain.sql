-- 04 flat-team: audit/cost survive user deletion (PRD §15.4, Q4).
-- Postgres can alter the FK in place (no table recreate needed).
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
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE audit_log ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';
UPDATE audit_log SET actor_email =
    COALESCE((SELECT email FROM users u WHERE u.id = audit_log.user_id), '(deleted-user)');

ALTER TABLE llm_request_cost ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';
UPDATE llm_request_cost SET actor_email =
    COALESCE((SELECT email FROM users u WHERE u.id = llm_request_cost.user_id), '(deleted-user)');
