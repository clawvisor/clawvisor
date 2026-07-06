package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Instance governance (spec 06a — local orggov). Postgres mirror of the
// sqlite implementation; see pkg/store/sqlite/store_governance.go for the
// append-only-history rationale.

// ── Model policy ──────────────────────────────────────────────────────────────

func (s *Store) GetActiveInstanceModelPolicy(ctx context.Context) (*store.InstanceModelPolicy, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, mode, models, created_by, created_at
		FROM instance_model_policy WHERE active = 1`)
	var p store.InstanceModelPolicy
	var modelsJSON string
	if err := row.Scan(&p.ID, &p.Mode, &modelsJSON, &p.CreatedBy, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(modelsJSON), &p.Models); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) PutInstanceModelPolicy(ctx context.Context, p *store.InstanceModelPolicy) error {
	modelsJSON, err := json.Marshal(p.Models)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE instance_model_policy SET active = 0 WHERE active = 1`); err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO instance_model_policy (id, mode, models, active, created_by)
		VALUES ($1, $2, $3, 1, $4)`, p.ID, p.Mode, string(modelsJSON), p.CreatedBy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ClearInstanceModelPolicy(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE instance_model_policy SET active = 0 WHERE active = 1`)
	return err
}

// ── Spend caps ──────────────────────────────────────────────────────────────

func (s *Store) ListInstanceSpendCaps(ctx context.Context) ([]*store.InstanceSpendCap, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, window_kind, cap_micros, enforcement, created_by, created_at, updated_at
		FROM instance_spend_cap ORDER BY window_kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InstanceSpendCap
	for rows.Next() {
		c, err := scanInstanceSpendCap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetInstanceSpendCap(ctx context.Context, windowKind string) (*store.InstanceSpendCap, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, window_kind, cap_micros, enforcement, created_by, created_at, updated_at
		FROM instance_spend_cap WHERE window_kind = $1`, windowKind)
	c, err := scanInstanceSpendCap(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return c, err
}

func (s *Store) PutInstanceSpendCap(ctx context.Context, c *store.InstanceSpendCap) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO instance_spend_cap (id, window_kind, cap_micros, enforcement, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (window_kind) DO UPDATE SET
			cap_micros = excluded.cap_micros,
			enforcement = excluded.enforcement,
			updated_at = NOW()`,
		c.ID, c.WindowKind, c.CapMicros, c.Enforcement, c.CreatedBy)
	return err
}

func (s *Store) DeleteInstanceSpendCap(ctx context.Context, windowKind string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM instance_spend_cap WHERE window_kind = $1`, windowKind)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SumInstanceCostMicros(ctx context.Context, since, until time.Time) (int64, error) {
	var sum int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0) FROM llm_request_cost
		WHERE timestamp >= $1 AND timestamp < $2`, since.UTC(), until.UTC()).Scan(&sum)
	return sum, err
}

func scanInstanceSpendCap(sc interface{ Scan(...any) error }) (*store.InstanceSpendCap, error) {
	var c store.InstanceSpendCap
	if err := sc.Scan(&c.ID, &c.WindowKind, &c.CapMicros, &c.Enforcement, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// ── Content policies ──────────────────────────────────────────────────────────

func (s *Store) ListInstanceContentPolicies(ctx context.Context) ([]*store.InstanceContentPolicy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, pattern, pattern_kind, action, block_message, enabled, created_by, created_at, updated_at
		FROM instance_content_policy ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InstanceContentPolicy
	for rows.Next() {
		p, err := scanInstanceContentPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetInstanceContentPolicy(ctx context.Context, id string) (*store.InstanceContentPolicy, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, pattern, pattern_kind, action, block_message, enabled, created_by, created_at, updated_at
		FROM instance_content_policy WHERE id = $1`, id)
	p, err := scanInstanceContentPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return p, err
}

func (s *Store) CreateInstanceContentPolicy(ctx context.Context, p *store.InstanceContentPolicy) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO instance_content_policy (id, name, pattern, pattern_kind, action, block_message, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.ID, p.Name, p.Pattern, p.PatternKind, p.Action, p.BlockMessage, boolToPGInt(p.Enabled), p.CreatedBy)
	return err
}

func (s *Store) UpdateInstanceContentPolicy(ctx context.Context, p *store.InstanceContentPolicy) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE instance_content_policy SET
			name = $1, pattern = $2, pattern_kind = $3, action = $4,
			block_message = $5, enabled = $6, updated_at = NOW()
		WHERE id = $7`,
		p.Name, p.Pattern, p.PatternKind, p.Action, p.BlockMessage, boolToPGInt(p.Enabled), p.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteInstanceContentPolicy(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM instance_content_policy WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanInstanceContentPolicy(sc interface{ Scan(...any) error }) (*store.InstanceContentPolicy, error) {
	var p store.InstanceContentPolicy
	var enabled int
	if err := sc.Scan(&p.ID, &p.Name, &p.Pattern, &p.PatternKind, &p.Action, &p.BlockMessage, &enabled, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	return &p, nil
}

// ── Task policy ──────────────────────────────────────────────────────────────

func (s *Store) GetActiveInstanceTaskPolicy(ctx context.Context) (*store.InstanceTaskPolicy, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, guidance, created_by, created_at
		FROM instance_task_policy WHERE active = 1`)
	var p store.InstanceTaskPolicy
	if err := row.Scan(&p.ID, &p.Guidance, &p.CreatedBy, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

func (s *Store) PutInstanceTaskPolicy(ctx context.Context, p *store.InstanceTaskPolicy) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE instance_task_policy SET active = 0 WHERE active = 1`); err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO instance_task_policy (id, guidance, active, created_by)
		VALUES ($1, $2, 1, $3)`, p.ID, p.Guidance, p.CreatedBy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ClearInstanceTaskPolicy(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE instance_task_policy SET active = 0 WHERE active = 1`)
	return err
}

// ── Violations ──────────────────────────────────────────────────────────────

func (s *Store) RecordInstancePolicyViolation(ctx context.Context, v *store.InstancePolicyViolation) error {
	if v.ID == "" {
		v.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO instance_policy_violation
			(id, user_id, agent_id, task_id, policy_kind, policy_id, action_taken, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		v.ID, pgNullIfEmpty(v.UserID), pgNullIfEmpty(v.AgentID), pgNullIfEmpty(v.TaskID),
		v.PolicyKind, pgNullIfEmpty(v.PolicyID), v.ActionTaken, pgNullIfEmpty(v.Detail))
	return err
}

func (s *Store) ListInstancePolicyViolations(ctx context.Context, limit int) ([]*store.InstancePolicyViolation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, agent_id, task_id, policy_kind, policy_id, action_taken, detail, created_at
		FROM instance_policy_violation ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InstancePolicyViolation
	for rows.Next() {
		var v store.InstancePolicyViolation
		var userID, agentID, taskID, policyID, detail *string
		if err := rows.Scan(&v.ID, &userID, &agentID, &taskID, &v.PolicyKind, &policyID, &v.ActionTaken, &detail, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.UserID = deref(userID)
		v.AgentID = deref(agentID)
		v.TaskID = deref(taskID)
		v.PolicyID = deref(policyID)
		v.Detail = deref(detail)
		out = append(out, &v)
	}
	return out, rows.Err()
}

func pgNullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToPGInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
