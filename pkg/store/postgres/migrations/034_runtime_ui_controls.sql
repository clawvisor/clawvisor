CREATE TABLE agent_runtime_settings (
    agent_id                 TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    runtime_enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    runtime_mode             TEXT NOT NULL DEFAULT 'observe',
    starter_profile          TEXT NOT NULL DEFAULT 'none',
    outbound_credential_mode TEXT NOT NULL DEFAULT 'inherit',
    inject_stored_bearer     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE runtime_policy_rules (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id           TEXT REFERENCES agents(id) ON DELETE CASCADE,
    kind               TEXT NOT NULL,
    action             TEXT NOT NULL,
    host               TEXT,
    method             TEXT,
    path               TEXT,
    path_regex         TEXT,
    headers_shape_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    body_shape_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    tool_name          TEXT,
    input_shape_json   JSONB NOT NULL DEFAULT '{}'::jsonb,
    input_regex        TEXT,
    reason             TEXT NOT NULL DEFAULT '',
    source             TEXT NOT NULL DEFAULT 'user',
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    last_matched_at    TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_runtime_policy_rules_lookup
    ON runtime_policy_rules(user_id, kind, enabled, agent_id, action);

CREATE TABLE runtime_preset_decisions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    command_key TEXT NOT NULL,
    profile     TEXT NOT NULL,
    decision    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_runtime_preset_decisions_user_command_profile
    ON runtime_preset_decisions(user_id, command_key, profile);
