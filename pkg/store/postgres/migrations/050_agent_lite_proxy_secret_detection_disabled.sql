ALTER TABLE agent_runtime_settings
ADD COLUMN IF NOT EXISTS lite_proxy_secret_detection_disabled BOOLEAN NOT NULL DEFAULT TRUE;
