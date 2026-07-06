-- 04 flat-team: admin/member roles + email-possession (verified_at).
--
-- One role column, two roles only (admin|member); default member. The
-- earliest-created REAL user becomes admin — this covers every existing
-- install (on upgrade the real users predate the migration-time `_instance`
-- / `__system__` seeds, so the earliest real row is genuinely the founder).
-- System rows (`_instance`, `__system__`, `admin@local`) are excluded from
-- the admin backfill exactly as they are from CountUsers.
--
-- The `_instance` row seeded by 05-lite (060) picks up role='member'
-- automatically via the DEFAULT — this spec adds NO `_instance` seed.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member';

UPDATE users SET role = 'admin'
WHERE id = (
    SELECT id FROM users
    WHERE id NOT IN ('__system__', '_instance') AND email != 'admin@local'
    ORDER BY created_at ASC, id ASC
    LIMIT 1
);

-- Email-possession state (invite security rule 2). NULL = unverified; an
-- account cannot log in or mint an agent token while unverified. There is
-- no pre-existing verification flag in OSS, so we add one. Every EXISTING
-- account (including the system rows) is backfilled verified so upgrades
-- never lock anyone out; only invite-claimed accounts created after this
-- migration start NULL and are flipped on magic-link confirm.
ALTER TABLE users ADD COLUMN verified_at TEXT;
UPDATE users SET verified_at = created_at WHERE verified_at IS NULL;
