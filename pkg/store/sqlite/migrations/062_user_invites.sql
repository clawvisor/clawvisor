-- 04 flat-team: single-use, short-lived invite tokens.
--
-- An invite is a bearer credential: only the SHA-256 hash lives at rest
-- (token_hash), the plaintext `cvinv_...` is returned exactly once on mint.
-- email='' means any email may claim; role is capped at member when claimed
-- over the enrollment channel (invite security rule 1). Default expiry is
-- 48h (rule 5); used_by/used_at pin single-use.
CREATE TABLE IF NOT EXISTS user_invites (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    email       TEXT NOT NULL DEFAULT '',      -- '' = any email may claim
    role        TEXT NOT NULL DEFAULT 'member',
    created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
    expires_at  TEXT NOT NULL,
    used_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
    used_at     TEXT,
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_user_invites_hash ON user_invites(token_hash);
