CREATE TABLE IF NOT EXISTS generated_adapters (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service_id   TEXT NOT NULL,
    yaml_content TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, service_id)
);
