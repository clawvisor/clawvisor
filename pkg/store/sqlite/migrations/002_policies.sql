CREATE TABLE policies (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug        TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    role_id     TEXT REFERENCES agent_roles(id) ON DELETE SET NULL,
    rules_yaml  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(user_id, slug)
);

CREATE INDEX idx_policies_user_id ON policies(user_id);
CREATE INDEX idx_policies_role_id ON policies(role_id);
