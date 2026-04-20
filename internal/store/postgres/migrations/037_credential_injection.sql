-- Stage 2: credential injection. See docs/design-proxy-stage2.md §3.

CREATE TABLE injectable_credentials (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_ref      TEXT NOT NULL,
    vault_key           TEXT NOT NULL,
    usable_by_agents    JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at          TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    UNIQUE (user_id, credential_ref)
);

CREATE INDEX idx_injectable_credentials_user_id ON injectable_credentials(user_id);

CREATE TABLE injection_rules (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT REFERENCES users(id) ON DELETE CASCADE,
    host_pattern        TEXT NOT NULL,
    path_pattern        TEXT NOT NULL DEFAULT '*',
    method              TEXT NOT NULL DEFAULT '*',
    inject_style        TEXT NOT NULL,
    inject_target       TEXT NOT NULL,
    inject_template     TEXT NOT NULL DEFAULT '{{credential}}',
    credential_ref      TEXT NOT NULL,
    priority            INTEGER NOT NULL DEFAULT 100,
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_injection_rules_user_id ON injection_rules(user_id);
CREATE INDEX idx_injection_rules_host ON injection_rules(host_pattern);

CREATE TABLE credential_usage_log (
    id                    BIGSERIAL PRIMARY KEY,
    ts                    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    agent_token_id        TEXT NOT NULL,
    credential_ref        TEXT NOT NULL,
    destination_host      TEXT NOT NULL DEFAULT '',
    destination_path      TEXT NOT NULL DEFAULT '',
    decision              TEXT NOT NULL,
    request_id            TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_credential_usage_agent ON credential_usage_log(agent_token_id, ts DESC);
CREATE INDEX idx_credential_usage_ref ON credential_usage_log(credential_ref, ts DESC);
