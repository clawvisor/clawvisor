-- Capture non-PII install facts the installer skill discovered about the
-- calling environment (harness, install mode, host OS, container ID, etc.).
-- Set at mint time, surfaced on the approval card, persisted with the request
-- for downstream debugging. Stored as JSON to keep the schema flat as new
-- fields are added; see pkg/store.InstallContext for the typed shape.
ALTER TABLE connection_requests
ADD COLUMN IF NOT EXISTS install_context TEXT NOT NULL DEFAULT '';
