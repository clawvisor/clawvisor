-- Per-proxy Ed25519 public signing keys. The proxy registers a new key at
-- startup (and on rotation, default daily); the server uses them to verify
-- TurnEvent signatures as audit metadata (Stage 1; ingest-time enforcement
-- begins at Stage 2).
--
-- See docs/design-proxy-stage1.md §4.3 and docs/proxy-api.md §5.3.
CREATE TABLE proxy_signing_keys (
    id                  TEXT PRIMARY KEY,            -- server-assigned
    proxy_instance_id   TEXT NOT NULL REFERENCES proxy_instances(id) ON DELETE CASCADE,
    key_id              TEXT NOT NULL,               -- proxy-supplied, unique per proxy instance
    alg                 TEXT NOT NULL,               -- 'ed25519'
    public_key          TEXT NOT NULL,               -- base64-encoded
    registered_at       TEXT NOT NULL DEFAULT (datetime('now')),
    retired_at          TEXT,
    UNIQUE (proxy_instance_id, key_id)
);

CREATE INDEX idx_proxy_signing_keys_proxy_instance_id ON proxy_signing_keys(proxy_instance_id);
