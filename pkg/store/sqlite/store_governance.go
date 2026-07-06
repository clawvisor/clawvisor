package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Instance governance (spec 06a — local orggov). Instance-scoped, one flat
// policy set. Model and task policies are append-only: Put demotes the
// prior active row (in a tx) and inserts a new one, so created_at history
// gives point-in-time answers for free.

// ── Model policy ──────────────────────────────────────────────────────────────

func (s *Store) GetActiveInstanceModelPolicy(ctx context.Context) (*store.InstanceModelPolicy, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, mode, models, created_by, created_at
		FROM instance_model_policy WHERE active = 1`)
	var p store.InstanceModelPolicy
	var modelsJSON, createdAt string
	if err := row.Scan(&p.ID, &p.Mode, &modelsJSON, &p.CreatedBy, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(modelsJSON), &p.Models); err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(createdAt)
	return &p, nil
}

func (s *Store) PutInstanceModelPolicy(ctx context.Context, p *store.InstanceModelPolicy) error {
	modelsJSON, err := json.Marshal(p.Models)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE instance_model_policy SET active = 0 WHERE active = 1`); err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO instance_model_policy (id, mode, models, active, created_by)
		VALUES (?, ?, ?, 1, ?)`, p.ID, p.Mode, string(modelsJSON), p.CreatedBy); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClearInstanceModelPolicy(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE instance_model_policy SET active = 0 WHERE active = 1`)
	return err
}

// ── Spend caps ──────────────────────────────────────────────────────────────

func (s *Store) ListInstanceSpendCaps(ctx context.Context) ([]*store.InstanceSpendCap, error) {
	rows, err := s.db.QueryContext(ctx, `
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
	row := s.db.QueryRowContext(ctx, `
		SELECT id, window_kind, cap_micros, enforcement, created_by, created_at, updated_at
		FROM instance_spend_cap WHERE window_kind = ?`, windowKind)
	c, err := scanInstanceSpendCap(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return c, err
}

func (s *Store) PutInstanceSpendCap(ctx context.Context, c *store.InstanceSpendCap) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instance_spend_cap (id, window_kind, cap_micros, enforcement, created_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(window_kind) DO UPDATE SET
			cap_micros = excluded.cap_micros,
			enforcement = excluded.enforcement,
			updated_at = datetime('now')`,
		c.ID, c.WindowKind, c.CapMicros, c.Enforcement, c.CreatedBy)
	return err
}

func (s *Store) DeleteInstanceSpendCap(ctx context.Context, windowKind string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM instance_spend_cap WHERE window_kind = ?`, windowKind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SumInstanceCostMicros(ctx context.Context, since, until time.Time) (int64, error) {
	var sum int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0) FROM llm_request_cost
		WHERE timestamp >= ? AND timestamp < ?`,
		since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)).Scan(&sum)
	return sum, err
}

func scanInstanceSpendCap(sc interface{ Scan(...any) error }) (*store.InstanceSpendCap, error) {
	var c store.InstanceSpendCap
	var createdAt, updatedAt string
	if err := sc.Scan(&c.ID, &c.WindowKind, &c.CapMicros, &c.Enforcement, &c.CreatedBy, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	return &c, nil
}

// ── Content policies ──────────────────────────────────────────────────────────

func (s *Store) ListInstanceContentPolicies(ctx context.Context) ([]*store.InstanceContentPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `
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
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, pattern, pattern_kind, action, block_message, enabled, created_by, created_at, updated_at
		FROM instance_content_policy WHERE id = ?`, id)
	p, err := scanInstanceContentPolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return p, err
}

func (s *Store) CreateInstanceContentPolicy(ctx context.Context, p *store.InstanceContentPolicy) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instance_content_policy (id, name, pattern, pattern_kind, action, block_message, enabled, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Pattern, p.PatternKind, p.Action, p.BlockMessage, boolToInt(p.Enabled), p.CreatedBy)
	return err
}

func (s *Store) UpdateInstanceContentPolicy(ctx context.Context, p *store.InstanceContentPolicy) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE instance_content_policy SET
			name = ?, pattern = ?, pattern_kind = ?, action = ?,
			block_message = ?, enabled = ?, updated_at = datetime('now')
		WHERE id = ?`,
		p.Name, p.Pattern, p.PatternKind, p.Action, p.BlockMessage, boolToInt(p.Enabled), p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteInstanceContentPolicy(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM instance_content_policy WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanInstanceContentPolicy(sc interface{ Scan(...any) error }) (*store.InstanceContentPolicy, error) {
	var p store.InstanceContentPolicy
	var enabled int
	var createdAt, updatedAt string
	if err := sc.Scan(&p.ID, &p.Name, &p.Pattern, &p.PatternKind, &p.Action, &p.BlockMessage, &enabled, &p.CreatedBy, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return &p, nil
}

// ── Task policy ──────────────────────────────────────────────────────────────

func (s *Store) GetActiveInstanceTaskPolicy(ctx context.Context) (*store.InstanceTaskPolicy, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, guidance, created_by, created_at
		FROM instance_task_policy WHERE active = 1`)
	var p store.InstanceTaskPolicy
	var createdAt string
	if err := row.Scan(&p.ID, &p.Guidance, &p.CreatedBy, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	p.CreatedAt = parseTime(createdAt)
	return &p, nil
}

func (s *Store) PutInstanceTaskPolicy(ctx context.Context, p *store.InstanceTaskPolicy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE instance_task_policy SET active = 0 WHERE active = 1`); err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO instance_task_policy (id, guidance, active, created_by)
		VALUES (?, ?, 1, ?)`, p.ID, p.Guidance, p.CreatedBy); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClearInstanceTaskPolicy(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE instance_task_policy SET active = 0 WHERE active = 1`)
	return err
}

// ── Violations ──────────────────────────────────────────────────────────────

func (s *Store) RecordInstancePolicyViolation(ctx context.Context, v *store.InstancePolicyViolation) error {
	if v.ID == "" {
		v.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instance_policy_violation
			(id, user_id, agent_id, task_id, policy_kind, policy_id, action_taken, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, nullIfEmpty(v.UserID), nullIfEmpty(v.AgentID), nullIfEmpty(v.TaskID),
		v.PolicyKind, nullIfEmpty(v.PolicyID), v.ActionTaken, nullIfEmpty(v.Detail))
	return err
}

func (s *Store) ListInstancePolicyViolations(ctx context.Context, limit int) ([]*store.InstancePolicyViolation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, agent_id, task_id, policy_kind, policy_id, action_taken, detail, created_at
		FROM instance_policy_violation ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InstancePolicyViolation
	for rows.Next() {
		var v store.InstancePolicyViolation
		var userID, agentID, taskID, policyID, detail sql.NullString
		var createdAt string
		if err := rows.Scan(&v.ID, &userID, &agentID, &taskID, &v.PolicyKind, &policyID, &v.ActionTaken, &detail, &createdAt); err != nil {
			return nil, err
		}
		v.UserID = userID.String
		v.AgentID = agentID.String
		v.TaskID = taskID.String
		v.PolicyID = policyID.String
		v.Detail = detail.String
		v.CreatedAt = parseTime(createdAt)
		out = append(out, &v)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
