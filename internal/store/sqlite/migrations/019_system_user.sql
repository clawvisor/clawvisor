-- Seed the __system__ user for system-level vault entries (e.g. Google OAuth
-- app credentials). The vault_entries table has a foreign key on users(id),
-- so this row must exist before writing system secrets.
INSERT OR IGNORE INTO users (id, email, password_hash)
VALUES ('__system__', '__system__@localhost', '');
