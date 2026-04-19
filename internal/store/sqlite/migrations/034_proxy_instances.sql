-- Proxy instances registered with this server. One row per paired Clawvisor Proxy.
-- Authenticated via cvisproxy_... token, scoped to ingest + config + signing-key endpoints.
--
-- See docs/design-proxy-stage1.md §3.1 and docs/proxy-api.md §4.2.
CREATE TABLE proxy_instances (
    id                     TEXT PRIMARY KEY,
    bridge_id              TEXT NOT NULL REFERENCES bridge_tokens(id) ON DELETE CASCADE,
    token_hash             TEXT NOT NULL UNIQUE,
    ca_cert_fingerprint    TEXT NOT NULL DEFAULT '',
    proxy_version          TEXT NOT NULL DEFAULT '',
    last_seen_at           TEXT,
    created_at             TEXT NOT NULL DEFAULT (datetime('now')),
    revoked_at             TEXT
);

CREATE INDEX idx_proxy_instances_bridge_id ON proxy_instances(bridge_id);
