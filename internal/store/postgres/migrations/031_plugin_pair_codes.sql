CREATE TABLE plugin_pair_codes (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);

CREATE INDEX idx_plugin_pair_codes_user_id ON plugin_pair_codes(user_id);

-- Idempotency key for pair requests: same code + same key → same pair_request id.
ALTER TABLE plugin_pair_requests ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX idx_plugin_pair_requests_idem
    ON plugin_pair_requests(user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
