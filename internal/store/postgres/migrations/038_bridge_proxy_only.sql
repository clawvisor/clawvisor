-- Stage 2 M4: proxy-only bridges — for Claude Code / Cursor users who
-- run the Network Proxy without an OpenClaw plugin. The bridge row
-- still exists (proxy configs and transcripts are scoped to bridges)
-- but no plugin-side secrets are issued.
ALTER TABLE bridge_tokens ADD COLUMN is_proxy_only BOOLEAN NOT NULL DEFAULT FALSE;
