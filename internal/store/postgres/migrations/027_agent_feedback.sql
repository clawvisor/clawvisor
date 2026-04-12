-- Agent feedback: bug reports and NPS responses
CREATE TABLE IF NOT EXISTS feedback_reports (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id    TEXT NOT NULL,
    agent_name  TEXT NOT NULL DEFAULT '',
    request_id  TEXT NOT NULL DEFAULT '',
    task_id     TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL DEFAULT 'other',
    description TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'medium',
    context     JSONB NOT NULL DEFAULT '{}',
    response    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_feedback_reports_user  ON feedback_reports(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_feedback_reports_agent ON feedback_reports(agent_id);

CREATE TABLE IF NOT EXISTS nps_responses (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id    TEXT NOT NULL,
    agent_name  TEXT NOT NULL DEFAULT '',
    task_id     TEXT NOT NULL DEFAULT '',
    score       INTEGER NOT NULL CHECK (score >= 1 AND score <= 10),
    feedback    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_nps_responses_agent ON nps_responses(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_nps_responses_user  ON nps_responses(user_id, created_at DESC);
