-- Track when a pending approval was claimed for execution so a daemon crash
-- mid-execution doesn't strand the row in 'executing' forever. The expiry
-- sweeper recovers rows whose lease has elapsed.
--
-- Stored as TEXT in 'YYYY-MM-DD HH:MM:SS' (UTC) to match the project's other
-- timestamp columns and to keep lexical comparison against datetime('now')
-- correct under the modernc sqlite driver.
ALTER TABLE pending_approvals ADD COLUMN executing_since TEXT;
