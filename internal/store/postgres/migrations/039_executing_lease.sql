-- Track when a pending approval was claimed for execution so a daemon crash
-- mid-execution doesn't strand the row in 'executing' forever. The expiry
-- sweeper recovers rows whose lease has elapsed.
ALTER TABLE pending_approvals ADD COLUMN executing_since TIMESTAMPTZ;
