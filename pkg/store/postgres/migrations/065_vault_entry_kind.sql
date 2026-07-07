-- Spec 10: reference-mode vault backends.
--
-- Discriminator column distinguishing a locally-encrypted pushed credential
-- ('push', the historical behaviour and default) from an external-secret
-- reference ('ref') — an AES-GCM-encrypted JSON envelope naming a secret in
-- the customer's own store (AWS/GCP Secret Manager), resolved to plaintext at
-- injection time and never persisted.
--
-- Existing rows default to 'push', so their read/lazy-AAD path is untouched.
ALTER TABLE vault_entries ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'push';
