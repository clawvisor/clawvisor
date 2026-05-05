-- Add daemon_id to authorization codes for relay-mode MCP pairing.
ALTER TABLE oauth_authorization_codes ADD COLUMN daemon_id TEXT NOT NULL DEFAULT '';
