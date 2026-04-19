-- Per-proxy Ed25519 public signing keys.
-- See docs/design-proxy-stage1.md §4.3 and docs/proxy-api.md §5.3.
CREATE TABLE proxy_signing_keys (
    id                  TEXT PRIMARY KEY,
    proxy_instance_id   TEXT NOT NULL REFERENCES proxy_instances(id) ON DELETE CASCADE,
    key_id              TEXT NOT NULL,
    alg                 TEXT NOT NULL,
    public_key          TEXT NOT NULL,
    registered_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at          TIMESTAMPTZ,
    UNIQUE (proxy_instance_id, key_id)
);

CREATE INDEX idx_proxy_signing_keys_proxy_instance_id ON proxy_signing_keys(proxy_instance_id);
