CREATE TABLE chain_facts (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_id  TEXT NOT NULL,
    audit_id    TEXT NOT NULL REFERENCES audit_log(id) ON DELETE CASCADE,
    service     TEXT NOT NULL,
    action      TEXT NOT NULL,
    fact_type   TEXT NOT NULL,
    fact_value  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_chain_facts_task_session ON chain_facts(task_id, session_id);
