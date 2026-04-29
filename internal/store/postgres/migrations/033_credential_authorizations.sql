CREATE TABLE credential_authorizations (
    id TEXT PRIMARY KEY,
    approval_id TEXT REFERENCES approval_records(id) ON DELETE SET NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    session_id TEXT REFERENCES runtime_sessions(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('once', 'session', 'standing')),
    credential_ref TEXT NOT NULL,
    service TEXT NOT NULL,
    host TEXT NOT NULL,
    header_name TEXT NOT NULL,
    scheme TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'used', 'revoked')),
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ,
    used_at TIMESTAMPTZ,
    last_matched_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_credential_authorizations_lookup
    ON credential_authorizations(
        user_id,
        agent_id,
        credential_ref,
        host,
        header_name,
        scheme,
        status,
        session_id,
        scope,
        expires_at
    );
