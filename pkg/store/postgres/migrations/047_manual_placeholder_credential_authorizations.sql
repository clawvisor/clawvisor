ALTER TABLE credential_authorizations ALTER COLUMN agent_id DROP NOT NULL;

DO $$
DECLARE
    constraint_name text;
BEGIN
    FOR constraint_name IN
        SELECT con.conname
        FROM pg_constraint con
        WHERE con.conrelid = 'credential_authorizations'::regclass
          AND con.contype = 'c'
          AND pg_get_constraintdef(con.oid) LIKE '%scope%'
    LOOP
        EXECUTE format('ALTER TABLE credential_authorizations DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;

ALTER TABLE credential_authorizations
    ADD CONSTRAINT credential_authorizations_scope_check
    CHECK (scope IN ('once', 'session', 'standing', 'manual'));
