-- Stage 3 M7: distinguish the two auto-approval paths in the judge
-- decisions audit log. 'proxy_flag_rule' = fast-policy flag rule sent
-- to the LLM judge. 'server_local_auto_approval' = Stage 0 task-level
-- auto-approval via intent.CheckApproval. 'unspecified' is the default
-- for pre-existing rows so the column can be added NOT NULL.
ALTER TABLE judge_decisions ADD COLUMN decision_path TEXT NOT NULL DEFAULT 'proxy_flag_rule';
