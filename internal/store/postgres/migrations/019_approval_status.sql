-- Add status column to pending_approvals for poll-then-execute approval flow.
-- Existing rows are implicitly "pending".
ALTER TABLE pending_approvals ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';
