-- 05-lite: minimal `_instance` system-user seed + `api_tokens` table.
--
-- Two statements, in order. FIRST the `_instance` seed, THEN the table —
-- the token middleware attributes token-authenticated writes to `_instance`
-- so resources created headlessly (Terraform / CI) aren't trapped in a
-- personal account (PRD §9.1). The seed writes only (id, email,
-- password_hash): no `role` column exists yet; spec 04 later adds
-- `role TEXT DEFAULT 'member'` which covers this row. The password_hash is
-- a non-bcrypt sentinel so the row can never authenticate via login.
--
-- 05-lite OWNS this seed (not 04). Do NOT split these across two numbered
-- files — that would consume 04's reserved migration block.
--
-- Use an id-targeted ON CONFLICT (matching the postgres/059 sibling) rather
-- than a blanket INSERT OR IGNORE: the latter swallows EVERY uniqueness
-- conflict (including the unique `email`), so a pre-existing row with the
-- same email but a different id would silently skip the `_instance` seed and
-- leave token-authenticated writes with no system user to attribute to.
INSERT INTO users (id, email, password_hash)
VALUES ('_instance', 'instance@system.clawvisor.invalid', '!locked!')
ON CONFLICT (id) DO NOTHING;

-- Long-lived, scoped, revocable API tokens (the Terraform provider / CI
-- credential). Plaintext is returned exactly once on create; only the
-- SHA-256 hash and a display prefix live at rest.
--
-- created_by ... ON DELETE SET NULL is deliberate: tokens are
-- instance-scoped and survive the deletion of the user who minted them.
-- is_bootstrap marks the short-lived first-boot bootstrap token so
-- burn-on-first-use can target it.
CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    scope        TEXT NOT NULL,                -- instance-admin (config-write|config-read land with 04)
    created_by   TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at   TEXT,                          -- NULL = no expiry (bootstrap always sets +24h)
    last_used_at TEXT,
    revoked_at   TEXT,
    is_bootstrap INTEGER NOT NULL DEFAULT 0     -- burn-on-use target (0/1)
);
CREATE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens(token_hash);
