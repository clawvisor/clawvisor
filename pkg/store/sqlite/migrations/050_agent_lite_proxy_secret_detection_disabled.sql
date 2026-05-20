ALTER TABLE agent_runtime_settings
ADD COLUMN lite_proxy_secret_detection_disabled INTEGER NOT NULL DEFAULT 1;
