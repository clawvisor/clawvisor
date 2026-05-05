-- 006_remove_roles.sql: Remove agent_roles table and role_id from agents.

ALTER TABLE agents DROP COLUMN IF EXISTS role_id;
DROP TABLE IF EXISTS agent_roles;
