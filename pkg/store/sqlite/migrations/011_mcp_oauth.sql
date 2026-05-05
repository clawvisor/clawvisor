-- Dynamic client registration (RFC 7591) for MCP OAuth 2.1
CREATE TABLE oauth_clients (
    id            TEXT PRIMARY KEY,
    client_name   TEXT NOT NULL,
    redirect_uris TEXT NOT NULL,  -- JSON array
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Authorization codes (one-time use, short-lived)
CREATE TABLE oauth_authorization_codes (
    code_hash      TEXT PRIMARY KEY,      -- SHA-256 of the code
    client_id      TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri   TEXT NOT NULL,
    code_challenge TEXT NOT NULL,          -- PKCE S256
    scope          TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
