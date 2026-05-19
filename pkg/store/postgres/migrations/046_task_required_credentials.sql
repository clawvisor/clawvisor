ALTER TABLE tasks ADD COLUMN required_credentials_json JSONB NOT NULL DEFAULT '[]';
