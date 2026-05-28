-- Per-agent setting for conversation-based auto-approval. See the
-- Postgres copy of this migration for the design notes. SQLite ALTER
-- TABLE doesn't support IF NOT EXISTS on columns, but the migration
-- runner is idempotent by version number, so re-running is harmless.
ALTER TABLE agent_runtime_settings
ADD COLUMN conversation_auto_approve_threshold TEXT NOT NULL DEFAULT 'off';
