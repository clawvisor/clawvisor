CREATE TABLE bridge_tokens (
    id                    TEXT PRIMARY KEY,
    user_id               TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash            TEXT NOT NULL UNIQUE,
    install_fingerprint   TEXT NOT NULL DEFAULT '',
    hostname              TEXT NOT NULL DEFAULT '',
    auto_approval_enabled INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at          TEXT,
    revoked_at            TEXT
);

CREATE INDEX idx_bridge_tokens_user_id ON bridge_tokens(user_id);

CREATE TABLE plugin_pair_requests (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    install_fingerprint TEXT NOT NULL DEFAULT '',
    hostname            TEXT NOT NULL DEFAULT '',
    agent_ids           TEXT NOT NULL DEFAULT '[]',
    status              TEXT NOT NULL DEFAULT 'pending',
    bridge_token_id     TEXT REFERENCES bridge_tokens(id) ON DELETE SET NULL,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at          TEXT NOT NULL
);

CREATE INDEX idx_plugin_pair_requests_user_id ON plugin_pair_requests(user_id);
CREATE INDEX idx_plugin_pair_requests_status ON plugin_pair_requests(status);
