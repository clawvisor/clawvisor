-- 04 flat-team: admin/member roles + email-possession (verified_at).
-- Postgres mirror of sqlite 061_user_roles.sql — identical semantics.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member';

UPDATE users SET role = 'admin'
WHERE id = (
    SELECT id FROM users
    WHERE id NOT IN ('__system__', '_instance') AND email != 'admin@local'
    ORDER BY created_at ASC, id ASC
    LIMIT 1
);

-- The magic-local operator is the instance admin (single-user local mode).
-- No-op on non-local installs. Matches run.go's fresh-install seed.
UPDATE users SET role = 'admin' WHERE email = 'admin@local';

-- Email-possession state (invite security rule 2). NULL = unverified.
-- Existing accounts are backfilled verified so upgrades never lock anyone
-- out; invite-claimed accounts start NULL and flip on magic-link confirm.
ALTER TABLE users ADD COLUMN verified_at TIMESTAMPTZ;
UPDATE users SET verified_at = created_at WHERE verified_at IS NULL;
