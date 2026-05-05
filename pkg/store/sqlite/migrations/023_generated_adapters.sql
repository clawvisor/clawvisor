CREATE TABLE IF NOT EXISTS generated_adapters (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service_id   TEXT NOT NULL,
    yaml_content TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, service_id)
);
