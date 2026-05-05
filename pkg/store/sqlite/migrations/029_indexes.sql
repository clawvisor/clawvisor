-- Indexes for common query patterns flagged by the 2026-04-15 audit.
-- All are CREATE INDEX IF NOT EXISTS so re-running is safe.

-- ListPendingApprovals filters by (user_id, status).
CREATE INDEX IF NOT EXISTS idx_pending_approvals_user_status
    ON pending_approvals(user_id, status);

-- Expiry cleanup for OAuth authorization codes scans by expires_at.
CREATE INDEX IF NOT EXISTS idx_oauth_auth_codes_expires_at
    ON oauth_authorization_codes(expires_at);

-- Task lookups by agent frequently filter by status in addition to agent_id.
-- The existing idx_tasks_agent (agent_id only) is retained; this composite
-- helps queries like "active tasks for agent X".
CREATE INDEX IF NOT EXISTS idx_tasks_agent_status
    ON tasks(agent_id, status);

-- gateway_request_log lookups by request_id (dedup checks, audits).
CREATE INDEX IF NOT EXISTS idx_request_log_request_id
    ON gateway_request_log(request_id);
