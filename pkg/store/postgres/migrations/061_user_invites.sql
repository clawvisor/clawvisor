-- 04 flat-team: single-use, short-lived invite tokens.
-- Postgres mirror of sqlite 062_user_invites.sql.
CREATE TABLE IF NOT EXISTS user_invites (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    email       TEXT NOT NULL DEFAULT '',      -- '' = any email may claim
    role        TEXT NOT NULL DEFAULT 'member',
    created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_user_invites_hash ON user_invites(token_hash);
