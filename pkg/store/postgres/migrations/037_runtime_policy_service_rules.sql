ALTER TABLE runtime_policy_rules ADD COLUMN service TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_policy_rules ADD COLUMN service_action TEXT NOT NULL DEFAULT '';

INSERT INTO runtime_policy_rules (
    id, user_id, agent_id, kind, action, service, service_action,
    reason, source, enabled, created_at, updated_at
)
SELECT
    id,
    user_id,
    NULL,
    'service',
    'deny',
    service,
    action,
    COALESCE(reason, ''),
    'migrated',
    TRUE,
    created_at,
    created_at
FROM restrictions r
WHERE NOT EXISTS (
    SELECT 1
    FROM runtime_policy_rules p
    WHERE p.user_id = r.user_id
      AND p.kind = 'service'
      AND COALESCE(p.service, '') = r.service
      AND COALESCE(p.service_action, '') = r.action
      AND p.action = 'deny'
);

CREATE UNIQUE INDEX idx_runtime_policy_service_rules_unique
    ON runtime_policy_rules(user_id, kind, COALESCE(agent_id, ''), action, service, service_action)
    WHERE kind = 'service';
