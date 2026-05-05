CREATE TABLE agent_runtime_settings (
    agent_id                   TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    runtime_enabled            INTEGER NOT NULL DEFAULT 1,
    runtime_mode               TEXT NOT NULL DEFAULT 'observe',
    starter_profile            TEXT NOT NULL DEFAULT 'none',
    outbound_credential_mode   TEXT NOT NULL DEFAULT 'inherit',
    inject_stored_bearer       INTEGER NOT NULL DEFAULT 0,
    created_at                 TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                 TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
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
    headers_shape_json TEXT NOT NULL DEFAULT '{}',
    body_shape_json    TEXT NOT NULL DEFAULT '{}',
    tool_name          TEXT,
    input_shape_json   TEXT NOT NULL DEFAULT '{}',
    input_regex        TEXT,
    reason             TEXT NOT NULL DEFAULT '',
    source             TEXT NOT NULL DEFAULT 'user',
    enabled            INTEGER NOT NULL DEFAULT 1,
    last_matched_at    TEXT,
    created_at         TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_runtime_policy_rules_lookup
    ON runtime_policy_rules(user_id, kind, enabled, agent_id, action);

CREATE TABLE runtime_preset_decisions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    command_key TEXT NOT NULL,
    profile     TEXT NOT NULL,
    decision    TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX idx_runtime_preset_decisions_user_command_profile
    ON runtime_preset_decisions(user_id, command_key, profile);
