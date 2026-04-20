package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) UpsertPolicy(ctx context.Context, p *store.Policy) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var existingID string
	var existingVersion int
	err = tx.QueryRow(ctx, `SELECT id, version FROM policies WHERE bridge_id = $1`, p.BridgeID).
		Scan(&existingID, &existingVersion)

	if errors.Is(err, pgx.ErrNoRows) {
		if p.ID == "" {
			p.ID = uuid.New().String()
		}
		p.Version = 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO policies (id, bridge_id, version, yaml, compiled_json, enabled)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, p.ID, p.BridgeID, p.Version, p.YAML, p.CompiledJSON, p.Enabled); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		p.ID = existingID
		p.Version = existingVersion + 1
		if _, err := tx.Exec(ctx, `
			UPDATE policies
			SET version = $1, yaml = $2, compiled_json = $3, enabled = $4, updated_at = NOW()
			WHERE id = $5
		`, p.Version, p.YAML, p.CompiledJSON, p.Enabled, p.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO policy_history (policy_id, bridge_id, version, yaml, author_user_id, comment)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, p.ID, p.BridgeID, p.Version, p.YAML, p.AuthorUserID, p.Comment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetPolicyByBridge(ctx context.Context, bridgeID string) (*store.Policy, error) {
	p := &store.Policy{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, bridge_id, version, yaml, compiled_json, enabled, created_at, updated_at
		FROM policies WHERE bridge_id = $1
	`, bridgeID).Scan(&p.ID, &p.BridgeID, &p.Version, &p.YAML, &p.CompiledJSON, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) ListPolicyHistory(ctx context.Context, policyID string, limit int) ([]*store.PolicyHistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, policy_id, bridge_id, version, yaml, author_user_id, comment, changed_at
		FROM policy_history WHERE policy_id = $1 ORDER BY version DESC LIMIT $2
	`, policyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.PolicyHistoryEntry
	for rows.Next() {
		e := &store.PolicyHistoryEntry{}
		if err := rows.Scan(&e.ID, &e.PolicyID, &e.BridgeID, &e.Version, &e.YAML, &e.AuthorUserID, &e.Comment, &e.ChangedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) InsertPolicyViolation(ctx context.Context, v *store.PolicyViolation) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO policy_violations
			(bridge_id, agent_token_id, rule_name, action, request_id, destination_host, destination_path, method, message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, v.BridgeID, v.AgentTokenID, v.RuleName, v.Action, v.RequestID, v.DestinationHost, v.DestinationPath, v.Method, v.Message)
	return err
}

func (s *Store) ListPolicyViolations(ctx context.Context, bridgeID string, since time.Time, limit int) ([]*store.PolicyViolation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, ts, bridge_id, agent_token_id, rule_name, action, request_id, destination_host, destination_path, method, message
		FROM policy_violations
		WHERE bridge_id = $1 AND ts >= $2
		ORDER BY ts DESC LIMIT $3
	`, bridgeID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.PolicyViolation
	for rows.Next() {
		v := &store.PolicyViolation{}
		if err := rows.Scan(&v.ID, &v.TS, &v.BridgeID, &v.AgentTokenID, &v.RuleName, &v.Action,
			&v.RequestID, &v.DestinationHost, &v.DestinationPath, &v.Method, &v.Message); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CountPolicyViolationsForAgent(ctx context.Context, bridgeID, agentTokenID, ruleName string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM policy_violations
		WHERE bridge_id = $1 AND agent_token_id = $2 AND rule_name = $3 AND action = 'block' AND ts >= $4
	`, bridgeID, agentTokenID, ruleName, since).Scan(&n)
	return n, err
}

func (s *Store) UpsertAgentBan(ctx context.Context, b *store.AgentBan) error {
	if b.ID == "" {
		b.ID = uuid.New().String()
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE agent_bans
		SET expires_at = $1, violation_count = $2, lifted_at = NULL, lifted_by = ''
		WHERE bridge_id = $3 AND agent_token_id = $4 AND rule_name = $5 AND lifted_at IS NULL
	`, b.ExpiresAt, b.ViolationCount, b.BridgeID, b.AgentTokenID, b.RuleName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO agent_bans (id, bridge_id, agent_token_id, rule_name, expires_at, violation_count)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, b.ID, b.BridgeID, b.AgentTokenID, b.RuleName, b.ExpiresAt, b.ViolationCount)
	return err
}

func (s *Store) ListActiveBans(ctx context.Context, bridgeID string) ([]*store.AgentBan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, bridge_id, agent_token_id, rule_name, banned_at, expires_at, violation_count, lifted_at, lifted_by
		FROM agent_bans
		WHERE bridge_id = $1 AND lifted_at IS NULL AND expires_at > NOW()
		ORDER BY banned_at DESC
	`, bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.AgentBan
	for rows.Next() {
		b := &store.AgentBan{}
		var liftedAt *time.Time
		if err := rows.Scan(&b.ID, &b.BridgeID, &b.AgentTokenID, &b.RuleName, &b.BannedAt, &b.ExpiresAt, &b.ViolationCount, &liftedAt, &b.LiftedBy); err != nil {
			return nil, err
		}
		b.LiftedAt = liftedAt
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) LiftAgentBan(ctx context.Context, bridgeID, agentTokenID, ruleName, liftedBy string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agent_bans
		SET lifted_at = NOW(), lifted_by = $1
		WHERE bridge_id = $2 AND agent_token_id = $3 AND rule_name = $4 AND lifted_at IS NULL
	`, liftedBy, bridgeID, agentTokenID, ruleName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) InsertJudgeDecision(ctx context.Context, d *store.JudgeDecision) error {
	path := d.DecisionPath
	if path == "" {
		path = "proxy_flag_rule"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO judge_decisions
			(bridge_id, agent_token_id, rule_name, cache_key, decision, reason, model, latency_ms, prompt_tokens, completion_tokens, decision_path)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, d.BridgeID, d.AgentTokenID, d.RuleName, d.CacheKey, d.Decision, d.Reason, d.Model, d.LatencyMs, d.PromptTokens, d.CompletionTokens, path)
	return err
}

func (s *Store) GetJudgeDecisionByCacheKey(ctx context.Context, cacheKey string, since time.Time) (*store.JudgeDecision, error) {
	d := &store.JudgeDecision{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, ts, bridge_id, agent_token_id, rule_name, cache_key, decision, reason, model, latency_ms, prompt_tokens, completion_tokens, decision_path
		FROM judge_decisions
		WHERE cache_key = $1 AND ts >= $2
		ORDER BY ts DESC LIMIT 1
	`, cacheKey, since).Scan(&d.ID, &d.TS, &d.BridgeID, &d.AgentTokenID, &d.RuleName, &d.CacheKey,
		&d.Decision, &d.Reason, &d.Model, &d.LatencyMs, &d.PromptTokens, &d.CompletionTokens, &d.DecisionPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return d, err
}
