ALTER TABLE credential_authorizations RENAME TO credential_authorizations_old;

CREATE TABLE credential_authorizations (
    id TEXT PRIMARY KEY,
    approval_id TEXT REFERENCES approval_records(id) ON DELETE SET NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id TEXT REFERENCES agents(id) ON DELETE CASCADE,
    session_id TEXT REFERENCES runtime_sessions(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('once', 'session', 'standing', 'manual')),
    credential_ref TEXT NOT NULL,
    service TEXT NOT NULL,
    host TEXT NOT NULL,
    header_name TEXT NOT NULL,
    scheme TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'used', 'revoked')),
    metadata_json TEXT NOT NULL DEFAULT '{}',
    expires_at TEXT,
    used_at TEXT,
    last_matched_at TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO credential_authorizations (
    id, approval_id, user_id, agent_id, session_id, scope, credential_ref,
    service, host, header_name, scheme, status, metadata_json, expires_at,
    used_at, last_matched_at, created_at
)
SELECT
    id, approval_id, user_id, agent_id, session_id, scope, credential_ref,
    service, host, header_name, scheme, status, metadata_json, expires_at,
    used_at, last_matched_at, created_at
FROM credential_authorizations_old;

DROP TABLE credential_authorizations_old;

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
