-- Capture non-PII install facts the installer skill discovered about the
-- calling environment (harness, install mode, host OS, container ID, etc.).
-- Set at mint time, surfaced on the approval card, copied onto the agent
-- record on approval so the dashboard can still show "this is an OpenClaw
-- install" after the connection request has dropped out of the pending list.
-- Stored as JSON to keep the schema flat as new fields are added; see
-- pkg/store.InstallContext for the typed shape.
ALTER TABLE connection_requests
ADD COLUMN IF NOT EXISTS install_context TEXT NOT NULL DEFAULT '';

ALTER TABLE agents
ADD COLUMN IF NOT EXISTS install_context TEXT NOT NULL DEFAULT '';
