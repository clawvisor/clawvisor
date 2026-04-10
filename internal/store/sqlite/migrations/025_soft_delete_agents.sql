-- Add soft-delete support for agents.
ALTER TABLE agents ADD COLUMN deleted_at TEXT;
