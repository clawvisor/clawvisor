-- Stage 3 M7: distinguish the two auto-approval paths in the judge
-- decisions audit log. See sqlite migration for semantics.
ALTER TABLE judge_decisions ADD COLUMN decision_path TEXT NOT NULL DEFAULT 'proxy_flag_rule';
