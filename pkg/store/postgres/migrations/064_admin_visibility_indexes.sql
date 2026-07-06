-- 04b admin visibility: supporting indexes for the cross-user (fleet-wide)
-- read paths. The existing audit_log / llm_request_cost indexes are all
-- user_id-prefixed (idx_audit_user_time, idx_llm_cost_user_time), so an
-- admin query that scans EVERY user ordered by time cannot use them and
-- degrades to a sequential scan + sort. These two plain time indexes back
-- the admin-only ListAllAuditEvents ordering and the InstanceCostSummary
-- time-window rollup. Members' user-scoped queries are unaffected (they keep
-- hitting the composite user_id-prefixed indexes).
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_llm_cost_time ON llm_request_cost(timestamp);
