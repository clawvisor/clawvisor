ALTER TABLE credential_authorizations ALTER COLUMN agent_id DROP NOT NULL;

ALTER TABLE credential_authorizations DROP CONSTRAINT IF EXISTS credential_authorizations_scope_check;
ALTER TABLE credential_authorizations
    ADD CONSTRAINT credential_authorizations_scope_check
    CHECK (scope IN ('once', 'session', 'standing', 'manual'));
