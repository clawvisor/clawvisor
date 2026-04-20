package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/google/uuid"
)

// UpsertPolicy inserts a new policy or replaces the existing one for a
// bridge, incrementing version and recording the previous YAML in
// policy_history. Runs in a transaction so a partial write can't leave
// the two tables out of sync.
func (s *Store) UpsertPolicy(ctx context.Context, p *store.Policy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var existingID string
	var existingVersion int
	err = tx.QueryRowContext(ctx,
		`SELECT id, version FROM policies WHERE bridge_id = ?`, p.BridgeID).
		Scan(&existingID, &existingVersion)

	if errors.Is(err, sql.ErrNoRows) {
		// First policy for this bridge.
		if p.ID == "" {
			p.ID = uuid.New().String()
		}
		p.Version = 1
		enabled := 0
		if p.Enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO policies (id, bridge_id, version, yaml, compiled_json, enabled)
			VALUES (?, ?, ?, ?, ?, ?)
		`, p.ID, p.BridgeID, p.Version, p.YAML, p.CompiledJSON, enabled); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		// Bump version + replace.
		p.ID = existingID
		p.Version = existingVersion + 1
		enabled := 0
		if p.Enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE policies
			SET version = ?, yaml = ?, compiled_json = ?, enabled = ?, updated_at = datetime('now')
			WHERE id = ?
		`, p.Version, p.YAML, p.CompiledJSON, enabled, p.ID); err != nil {
			return err
		}
	}

	// Always record the new version in history.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO policy_history (policy_id, bridge_id, version, yaml, author_user_id, comment)
		VALUES (?, ?, ?, ?, ?, ?)
	`, p.ID, p.BridgeID, p.Version, p.YAML, p.AuthorUserID, p.Comment); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetPolicyByBridge(ctx context.Context, bridgeID string) (*store.Policy, error) {
	p := &store.Policy{}
	var enabled int
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, bridge_id, version, yaml, compiled_json, enabled, created_at, updated_at
		FROM policies WHERE bridge_id = ?
	`, bridgeID).Scan(&p.ID, &p.BridgeID, &p.Version, &p.YAML, &p.CompiledJSON, &enabled, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return p, nil
}

func (s *Store) ListPolicyHistory(ctx context.Context, policyID string, limit int) ([]*store.PolicyHistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, policy_id, bridge_id, version, yaml, author_user_id, comment, changed_at
		FROM policy_history WHERE policy_id = ? ORDER BY version DESC LIMIT ?
	`, policyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.PolicyHistoryEntry
	for rows.Next() {
		e := &store.PolicyHistoryEntry{}
		var changedAt string
		if err := rows.Scan(&e.ID, &e.PolicyID, &e.BridgeID, &e.Version, &e.YAML, &e.AuthorUserID, &e.Comment, &changedAt); err != nil {
			return nil, err
		}
		e.ChangedAt = parseTime(changedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) InsertPolicyViolation(ctx context.Context, v *store.PolicyViolation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO policy_violations
			(bridge_id, agent_token_id, rule_name, action, request_id, destination_host, destination_path, method, message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, v.BridgeID, v.AgentTokenID, v.RuleName, v.Action, v.RequestID, v.DestinationHost, v.DestinationPath, v.Method, v.Message)
	return err
}

func (s *Store) ListPolicyViolations(ctx context.Context, bridgeID string, since time.Time, limit int) ([]*store.PolicyViolation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, bridge_id, agent_token_id, rule_name, action, request_id, destination_host, destination_path, method, message
		FROM policy_violations
		WHERE bridge_id = ? AND ts >= ?
		ORDER BY ts DESC LIMIT ?
	`, bridgeID, since.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.PolicyViolation
	for rows.Next() {
		v := &store.PolicyViolation{}
		var ts string
		if err := rows.Scan(&v.ID, &ts, &v.BridgeID, &v.AgentTokenID, &v.RuleName, &v.Action,
			&v.RequestID, &v.DestinationHost, &v.DestinationPath, &v.Method, &v.Message); err != nil {
			return nil, err
		}
		v.TS = parseTime(ts)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CountPolicyViolationsForAgent(ctx context.Context, bridgeID, agentTokenID, ruleName string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM policy_violations
		WHERE bridge_id = ? AND agent_token_id = ? AND rule_name = ? AND action = 'block' AND ts >= ?
	`, bridgeID, agentTokenID, ruleName, since.UTC().Format(time.RFC3339)).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) UpsertAgentBan(ctx context.Context, b *store.AgentBan) error {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	// Any currently-active ban for the same (bridge, agent, rule) gets
	// extended by replacing its expires_at + violation_count.
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_bans
		SET expires_at = ?, violation_count = ?, lifted_at = NULL, lifted_by = ''
		WHERE bridge_id = ? AND agent_token_id = ? AND rule_name = ? AND lifted_at IS NULL
	`, b.ExpiresAt.UTC().Format(time.RFC3339), b.ViolationCount, b.BridgeID, b.AgentTokenID, b.RuleName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_bans (id, bridge_id, agent_token_id, rule_name, expires_at, violation_count)
		VALUES (?, ?, ?, ?, ?, ?)
	`, b.ID, b.BridgeID, b.AgentTokenID, b.RuleName, b.ExpiresAt.UTC().Format(time.RFC3339), b.ViolationCount)
	return err
}

func (s *Store) ListActiveBans(ctx context.Context, bridgeID string) ([]*store.AgentBan, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, bridge_id, agent_token_id, rule_name, banned_at, expires_at, violation_count, lifted_at, lifted_by
		FROM agent_bans
		WHERE bridge_id = ? AND lifted_at IS NULL AND expires_at > ?
		ORDER BY banned_at DESC
	`, bridgeID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.AgentBan
	for rows.Next() {
		b := &store.AgentBan{}
		var bannedAt, expiresAt string
		var liftedAt sql.NullString
		if err := rows.Scan(&b.ID, &b.BridgeID, &b.AgentTokenID, &b.RuleName, &bannedAt, &expiresAt, &b.ViolationCount, &liftedAt, &b.LiftedBy); err != nil {
			return nil, err
		}
		b.BannedAt = parseTime(bannedAt)
		b.ExpiresAt = parseTime(expiresAt)
		if liftedAt.Valid {
			t := parseTime(liftedAt.String)
			b.LiftedAt = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) LiftAgentBan(ctx context.Context, bridgeID, agentTokenID, ruleName, liftedBy string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_bans
		SET lifted_at = datetime('now'), lifted_by = ?
		WHERE bridge_id = ? AND agent_token_id = ? AND rule_name = ? AND lifted_at IS NULL
	`, liftedBy, bridgeID, agentTokenID, ruleName)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) InsertJudgeDecision(ctx context.Context, d *store.JudgeDecision) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO judge_decisions
			(bridge_id, agent_token_id, rule_name, cache_key, decision, reason, model, latency_ms, prompt_tokens, completion_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, d.BridgeID, d.AgentTokenID, d.RuleName, d.CacheKey, d.Decision, d.Reason, d.Model, d.LatencyMs, d.PromptTokens, d.CompletionTokens)
	return err
}

func (s *Store) GetJudgeDecisionByCacheKey(ctx context.Context, cacheKey string, since time.Time) (*store.JudgeDecision, error) {
	d := &store.JudgeDecision{}
	var ts string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, ts, bridge_id, agent_token_id, rule_name, cache_key, decision, reason, model, latency_ms, prompt_tokens, completion_tokens
		FROM judge_decisions
		WHERE cache_key = ? AND ts >= ?
		ORDER BY ts DESC LIMIT 1
	`, cacheKey, since.UTC().Format(time.RFC3339)).Scan(&d.ID, &ts, &d.BridgeID, &d.AgentTokenID, &d.RuleName, &d.CacheKey,
		&d.Decision, &d.Reason, &d.Model, &d.LatencyMs, &d.PromptTokens, &d.CompletionTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.TS = parseTime(ts)
	return d, nil
}
