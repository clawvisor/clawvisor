-- Stage 2: credential injection. Vault extensions + injection rules + audit.
-- See docs/design-proxy-stage2.md §3.

-- Per-credential metadata the vault alone doesn't have: the "credential_ref"
-- identifier injection rules match against, plus a per-credential ACL
-- ("which agents can use this?"). One row per (user_id, credential_ref).
-- The actual encrypted secret lives in the existing vault; this table
-- points at it + adds Stage 2 metadata.
CREATE TABLE injectable_credentials (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_ref      TEXT NOT NULL,          -- 'vault:anthropic', 'vault:openai', 'vault:telegram-bot', …
    vault_key           TEXT NOT NULL,          -- pointer into the vault (the key under which the encrypted blob is stored)
    usable_by_agents    TEXT NOT NULL DEFAULT '',  -- JSON array of cvis_ token ids; empty = "any agent of this bridge"
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    rotated_at          TEXT,
    revoked_at          TEXT,
    UNIQUE (user_id, credential_ref)
);

CREATE INDEX idx_injectable_credentials_user_id ON injectable_credentials(user_id);

-- Injection rules: global (user_id = NULL) for built-in services, plus
-- optional user-level overrides. Proxy evaluates these in priority order
-- and matches on host/path. The matched rule names a credential_ref;
-- server looks that up in injectable_credentials for the user, decrypts
-- via vault, returns to proxy for injection.
CREATE TABLE injection_rules (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT REFERENCES users(id) ON DELETE CASCADE,  -- NULL = built-in
    host_pattern        TEXT NOT NULL,
    path_pattern        TEXT NOT NULL DEFAULT '*',
    method              TEXT NOT NULL DEFAULT '*',
    inject_style        TEXT NOT NULL,        -- 'header' | 'path' | 'query'
    inject_target       TEXT NOT NULL,        -- header name / query key / path placeholder
    inject_template     TEXT NOT NULL DEFAULT '{{credential}}',
    credential_ref      TEXT NOT NULL,
    priority            INTEGER NOT NULL DEFAULT 100,
    enabled             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_injection_rules_user_id ON injection_rules(user_id);
CREATE INDEX idx_injection_rules_host ON injection_rules(host_pattern);

-- Per-lookup audit record. Proxy asks for a credential → server logs
-- who asked, for what, when. Useful for dashboards and forensics.
CREATE TABLE credential_usage_log (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                    TEXT NOT NULL DEFAULT (datetime('now')),
    agent_token_id        TEXT NOT NULL,
    credential_ref        TEXT NOT NULL,
    destination_host      TEXT NOT NULL DEFAULT '',
    destination_path      TEXT NOT NULL DEFAULT '',
    decision              TEXT NOT NULL,       -- 'granted' | 'denied_acl' | 'denied_revoked' | 'not_found'
    request_id            TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_credential_usage_agent ON credential_usage_log(agent_token_id, ts DESC);
CREATE INDEX idx_credential_usage_ref ON credential_usage_log(credential_ref, ts DESC);
