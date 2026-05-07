# SQLite Migrations

Migrations are applied in lexical order at daemon startup by `pkg/store/sqlite.New`.

## Avoid full-table rewrites

SQLite holds an exclusive write lock for the duration of any `INSERT INTO new SELECT FROM old` cycle, so a migration that rewrites a large table (audit log, chain facts, etc.) blocks every other writer until it commits. On a self-host install with months of audit history this can manifest as the daemon "hanging" silently for the user the first time they upgrade.

`028_audit_request_id_user_unique.sql` is an existing example of this pattern. It cannot be reverted in place because users have already run it; future schema changes that need to alter constraints on `audit_log` (or any other large table) should:

1. Add the new column / index out-of-band where possible.
2. If a structural change is unavoidable, do it in chunks — for example, write a one-shot Go migration that processes 10k rows per transaction and surfaces progress through the daemon log.
3. At minimum, document the expected blocking time in the migration's leading comment so the operator knows what to expect.

## Numbering collisions

Migration numbers must be unique within this directory. The historical `019_*` and `027_*` collisions are tolerated for backwards compatibility but should not be repeated. Use `ls -1 | sort -V | tail` before adding a new migration to confirm the next free number.
