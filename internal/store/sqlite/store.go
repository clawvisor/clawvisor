package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ericlevine/clawvisor/internal/store"
)

// Store implements store.Store using SQLite via database/sql.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB (used by LocalVault).
func (s *Store) DB() *sql.DB {
	return s.db
}

// ── Users ─────────────────────────────────────────────────────────────────────

func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (*store.User, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES (?, ?, ?)`,
		id, email, passwordHash,
	)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return s.GetUserByID(ctx, id)
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	u := &store.User{}
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at, updated_at FROM users WHERE email = ?`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return u, nil
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*store.User, error) {
	u := &store.User{}
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at, updated_at FROM users WHERE id = ?`,
		id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return u, nil
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID, newPasswordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		newPasswordHash, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ── Agent Roles ───────────────────────────────────────────────────────────────

func (s *Store) CreateRole(ctx context.Context, userID, name, description string) (*store.AgentRole, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_roles (id, user_id, name, description) VALUES (?, ?, ?, ?)`,
		id, userID, name, description,
	)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return s.GetRole(ctx, id, userID)
}

func (s *Store) GetRole(ctx context.Context, id, userID string) (*store.AgentRole, error) {
	r := &store.AgentRole{}
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, description, created_at FROM agent_roles WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}

func (s *Store) ListRoles(ctx context.Context, userID string) ([]*store.AgentRole, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, name, description, created_at FROM agent_roles WHERE user_id = ? ORDER BY name`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []*store.AgentRole
	for rows.Next() {
		r := &store.AgentRole{}
		var createdAt string
		if err := rows.Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(createdAt)
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

func (s *Store) UpdateRole(ctx context.Context, id, userID, name, description string) (*store.AgentRole, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_roles SET name = ?, description = ? WHERE id = ? AND user_id = ?`,
		name, description, id, userID,
	)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetRole(ctx, id, userID)
}

func (s *Store) GetRoleByName(ctx context.Context, name, userID string) (*store.AgentRole, error) {
	r := &store.AgentRole{}
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, description, created_at FROM agent_roles WHERE name = ? AND user_id = ?`,
		name, userID,
	).Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}

func (s *Store) DeleteRole(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_roles WHERE id = ? AND user_id = ?`,
		id, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ── Policies ──────────────────────────────────────────────────────────────────

func (s *Store) CreatePolicy(ctx context.Context, userID string, p *store.PolicyRecord) (*store.PolicyRecord, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO policies (id, user_id, slug, name, description, role_id, rules_yaml)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, userID, p.Slug, p.Name, p.Description, p.RoleID, p.RulesYAML)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return s.GetPolicy(ctx, id, userID)
}

func (s *Store) UpdatePolicy(ctx context.Context, id, userID string, p *store.PolicyRecord) (*store.PolicyRecord, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE policies
		SET slug = ?, name = ?, description = ?, role_id = ?, rules_yaml = ?, updated_at = datetime('now')
		WHERE id = ? AND user_id = ?
	`, p.Slug, p.Name, p.Description, p.RoleID, p.RulesYAML, id, userID)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetPolicy(ctx, id, userID)
}

func (s *Store) DeletePolicy(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM policies WHERE id = ? AND user_id = ?`,
		id, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetPolicy(ctx context.Context, id, userID string) (*store.PolicyRecord, error) {
	p := &store.PolicyRecord{}
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, slug, name, description, role_id, rules_yaml, created_at, updated_at
		FROM policies WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&p.ID, &p.UserID, &p.Slug, &p.Name, &p.Description, &p.RoleID, &p.RulesYAML, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return p, nil
}

func (s *Store) GetPolicyBySlug(ctx context.Context, slug, userID string) (*store.PolicyRecord, error) {
	p := &store.PolicyRecord{}
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, slug, name, description, role_id, rules_yaml, created_at, updated_at
		FROM policies WHERE slug = ? AND user_id = ?
	`, slug, userID).Scan(&p.ID, &p.UserID, &p.Slug, &p.Name, &p.Description, &p.RoleID, &p.RulesYAML, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return p, nil
}

func (s *Store) ListPolicies(ctx context.Context, userID string, filter store.PolicyFilter) ([]*store.PolicyRecord, error) {
	query := `
		SELECT id, user_id, slug, name, description, role_id, rules_yaml, created_at, updated_at
		FROM policies WHERE user_id = ?`
	args := []any{userID}

	if filter.GlobalOnly {
		query += ` AND role_id IS NULL`
	} else if filter.RoleID != nil {
		query += ` AND role_id = ?`
		args = append(args, *filter.RoleID)
	}
	query += ` ORDER BY created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []*store.PolicyRecord
	for rows.Next() {
		p := &store.PolicyRecord{}
		var createdAt, updatedAt string
		if err := rows.Scan(&p.ID, &p.UserID, &p.Slug, &p.Name, &p.Description, &p.RoleID, &p.RulesYAML, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTime(createdAt)
		p.UpdatedAt = parseTime(updatedAt)
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

// ── Agents ────────────────────────────────────────────────────────────────────

func (s *Store) CreateAgent(ctx context.Context, userID, name, tokenHash string, roleID *string) (*store.Agent, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, token_hash, role_id) VALUES (?, ?, ?, ?, ?)`,
		id, userID, name, tokenHash, roleID,
	)
	if err != nil {
		return nil, err
	}
	return s.getAgentByID(ctx, id)
}

func (s *Store) UpdateAgentRole(ctx context.Context, id, userID string, roleID *string) (*store.Agent, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET role_id = ? WHERE id = ? AND user_id = ?`,
		roleID, id, userID,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, store.ErrNotFound
	}
	return s.getAgentByID(ctx, id)
}

func (s *Store) GetAgentByToken(ctx context.Context, tokenHash string) (*store.Agent, error) {
	a := &store.Agent{}
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, role_id, created_at FROM agents WHERE token_hash = ?`,
		tokenHash,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.RoleID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = parseTime(createdAt)
	return a, nil
}

func (s *Store) ListAgents(ctx context.Context, userID string) ([]*store.Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, name, token_hash, role_id, created_at FROM agents WHERE user_id = ? ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []*store.Agent
	for rows.Next() {
		a := &store.Agent{}
		var createdAt string
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.RoleID, &createdAt); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(createdAt)
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *Store) DeleteAgent(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agents WHERE id = ? AND user_id = ?`,
		id, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) getAgentByID(ctx context.Context, id string) (*store.Agent, error) {
	a := &store.Agent{}
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, role_id, created_at FROM agents WHERE id = ?`,
		id,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.RoleID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = parseTime(createdAt)
	return a, nil
}

// ── Sessions ──────────────────────────────────────────────────────────────────

func (s *Store) CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (*store.Session, error) {
	sess := &store.Session{
		ID:        uuid.New().String(),
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, token_hash, expires_at) VALUES (?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.ExpiresAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) GetSession(ctx context.Context, tokenHash string) (*store.Session, error) {
	sess := &store.Session{}
	var expiresAt, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, token_hash, expires_at, created_at FROM sessions WHERE token_hash = ?`,
		tokenHash,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &expiresAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.ExpiresAt = parseTime(expiresAt)
	sess.CreatedAt = parseTime(createdAt)
	return sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// ── Service Metadata ──────────────────────────────────────────────────────────

func (s *Store) UpsertServiceMeta(ctx context.Context, userID, serviceID string, activatedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_meta (id, user_id, service_id, activated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, service_id) DO UPDATE SET
			activated_at = excluded.activated_at,
			updated_at   = CURRENT_TIMESTAMP
	`, uuid.New().String(), userID, serviceID, activatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetServiceMeta(ctx context.Context, userID, serviceID string) (*store.ServiceMeta, error) {
	m := &store.ServiceMeta{}
	var activatedAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, service_id, activated_at, updated_at FROM service_meta WHERE user_id = ? AND service_id = ?`,
		userID, serviceID,
	).Scan(&m.ID, &m.UserID, &m.ServiceID, &activatedAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.ActivatedAt = parseTime(activatedAt)
	m.UpdatedAt = parseTime(updatedAt)
	return m, nil
}

func (s *Store) ListServiceMetas(ctx context.Context, userID string) ([]*store.ServiceMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, service_id, activated_at, updated_at FROM service_meta WHERE user_id = ? ORDER BY service_id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metas []*store.ServiceMeta
	for rows.Next() {
		m := &store.ServiceMeta{}
		var activatedAt, updatedAt string
		if err := rows.Scan(&m.ID, &m.UserID, &m.ServiceID, &activatedAt, &updatedAt); err != nil {
			return nil, err
		}
		m.ActivatedAt = parseTime(activatedAt)
		m.UpdatedAt = parseTime(updatedAt)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func (s *Store) DeleteServiceMeta(ctx context.Context, userID, serviceID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM service_meta WHERE user_id = ? AND service_id = ?`,
		userID, serviceID,
	)
	return err
}

// ── Notification Configs ──────────────────────────────────────────────────────

func (s *Store) UpsertNotificationConfig(ctx context.Context, userID, channel string, config json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_configs (id, user_id, channel, config)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, channel) DO UPDATE SET
			config     = excluded.config,
			updated_at = CURRENT_TIMESTAMP
	`, uuid.New().String(), userID, channel, string(config))
	return err
}

func (s *Store) GetNotificationConfig(ctx context.Context, userID, channel string) (*store.NotificationConfig, error) {
	nc := &store.NotificationConfig{}
	var configStr, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, channel, config, created_at, updated_at FROM notification_configs WHERE user_id = ? AND channel = ?`,
		userID, channel,
	).Scan(&nc.ID, &nc.UserID, &nc.Channel, &configStr, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	nc.Config = json.RawMessage(configStr)
	nc.CreatedAt = parseTime(createdAt)
	nc.UpdatedAt = parseTime(updatedAt)
	return nc, nil
}

// ── Audit Log ─────────────────────────────────────────────────────────────────

func (s *Store) LogAudit(ctx context.Context, e *store.AuditEntry) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	paramsSafe := "{}"
	if len(e.ParamsSafe) > 0 {
		paramsSafe = string(e.ParamsSafe)
	}
	var filtersApplied *string
	if len(e.FiltersApplied) > 0 {
		s := string(e.FiltersApplied)
		filtersApplied = &s
	}
	safetyFlagged := 0
	if e.SafetyFlagged {
		safetyFlagged = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (
			id, user_id, agent_id, request_id, timestamp, service, action,
			params_safe, decision, outcome, policy_id, rule_id,
			safety_flagged, safety_reason, reason, data_origin, context_src,
			duration_ms, filters_applied, error_msg
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, e.ID, e.UserID, e.AgentID, e.RequestID, e.Timestamp.UTC().Format(time.RFC3339),
		e.Service, e.Action, paramsSafe, e.Decision, e.Outcome,
		e.PolicyID, e.RuleID, safetyFlagged, e.SafetyReason, e.Reason,
		e.DataOrigin, e.ContextSrc, e.DurationMS, filtersApplied, e.ErrorMsg)
	return err
}

func (s *Store) UpdateAuditOutcome(ctx context.Context, id, outcome, errMsg string, durationMS int) error {
	var errMsgPtr *string
	if errMsg != "" {
		errMsgPtr = &errMsg
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE audit_log SET outcome = ?, error_msg = ?, duration_ms = ? WHERE id = ?`,
		outcome, errMsgPtr, durationMS, id)
	return err
}

func (s *Store) GetAuditEntry(ctx context.Context, id, userID string) (*store.AuditEntry, error) {
	e := &store.AuditEntry{}
	var timestamp, paramsSafe string
	var safetyFlagged int
	var filtersApplied *string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, agent_id, request_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, error_msg
		FROM audit_log WHERE id = ? AND user_id = ?
	`, id, userID).Scan(
		&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &timestamp,
		&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
		&e.PolicyID, &e.RuleID, &safetyFlagged, &e.SafetyReason, &e.Reason,
		&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &e.ErrorMsg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Timestamp = parseTime(timestamp)
	e.SafetyFlagged = safetyFlagged != 0
	e.ParamsSafe = json.RawMessage(paramsSafe)
	if filtersApplied != nil {
		e.FiltersApplied = json.RawMessage(*filtersApplied)
	}
	return e, nil
}

func (s *Store) ListAuditEntries(ctx context.Context, userID string, filter store.AuditFilter) ([]*store.AuditEntry, int, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	where := "WHERE user_id = ?"
	args := []any{userID}

	if filter.Service != "" {
		where += " AND service = ?"
		args = append(args, filter.Service)
	}
	if filter.Outcome != "" {
		where += " AND outcome = ?"
		args = append(args, filter.Outcome)
	}
	if filter.DataOrigin != "" {
		where += " AND data_origin = ?"
		args = append(args, filter.DataOrigin)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	dataArgs := append(args, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, agent_id, request_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, error_msg
		FROM audit_log `+where+` ORDER BY timestamp DESC LIMIT ? OFFSET ?`, dataArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []*store.AuditEntry
	for rows.Next() {
		e := &store.AuditEntry{}
		var timestamp, paramsSafe string
		var safetyFlagged int
		var filtersApplied *string
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &timestamp,
			&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
			&e.PolicyID, &e.RuleID, &safetyFlagged, &e.SafetyReason, &e.Reason,
			&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &e.ErrorMsg,
		); err != nil {
			return nil, 0, err
		}
		e.Timestamp = parseTime(timestamp)
		e.SafetyFlagged = safetyFlagged != 0
		e.ParamsSafe = json.RawMessage(paramsSafe)
		if filtersApplied != nil {
			e.FiltersApplied = json.RawMessage(*filtersApplied)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// ── Pending Approvals ─────────────────────────────────────────────────────────

func (s *Store) SavePendingApproval(ctx context.Context, pa *store.PendingApproval) error {
	if pa.ID == "" {
		pa.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_approvals (id, user_id, request_id, audit_id, request_blob, callback_url, telegram_msg_id, expires_at)
		VALUES (?,?,?,?,?,?,?,?)
	`, pa.ID, pa.UserID, pa.RequestID, pa.AuditID, string(pa.RequestBlob),
		pa.CallbackURL, pa.TelegramMsgID, pa.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetPendingApproval(ctx context.Context, requestID string) (*store.PendingApproval, error) {
	pa := &store.PendingApproval{}
	var requestBlob, expiresAt, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, telegram_msg_id, expires_at, created_at
		FROM pending_approvals WHERE request_id = ?
	`, requestID).Scan(
		&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
		&pa.CallbackURL, &pa.TelegramMsgID, &expiresAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pa.RequestBlob = json.RawMessage(requestBlob)
	pa.ExpiresAt = parseTime(expiresAt)
	pa.CreatedAt = parseTime(createdAt)
	return pa, nil
}

func (s *Store) UpdatePendingTelegramMsgID(ctx context.Context, requestID, msgID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pending_approvals SET telegram_msg_id = ? WHERE request_id = ?`,
		msgID, requestID)
	return err
}

func (s *Store) DeletePendingApproval(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_approvals WHERE request_id = ?`, requestID)
	return err
}

func (s *Store) ListExpiredPendingApprovals(ctx context.Context) ([]*store.PendingApproval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, telegram_msg_id, expires_at, created_at
		FROM pending_approvals WHERE expires_at < datetime('now')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pas []*store.PendingApproval
	for rows.Next() {
		pa := &store.PendingApproval{}
		var requestBlob, expiresAt, createdAt string
		if err := rows.Scan(
			&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
			&pa.CallbackURL, &pa.TelegramMsgID, &expiresAt, &createdAt,
		); err != nil {
			return nil, err
		}
		pa.RequestBlob = json.RawMessage(requestBlob)
		pa.ExpiresAt = parseTime(expiresAt)
		pa.CreatedAt = parseTime(createdAt)
		pas = append(pas, pa)
	}
	return pas, rows.Err()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

// parseTime parses SQLite TEXT timestamps in multiple formats.
func parseTime(s string) time.Time {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Ensure Store implements store.Store at compile time.
var _ store.Store = (*Store)(nil)

// unused import guard
var _ = fmt.Sprintf
