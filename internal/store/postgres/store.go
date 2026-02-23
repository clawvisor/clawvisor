package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericlevine/clawvisor/internal/store"
)

// Store implements store.Store using a Postgres pgxpool.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping verifies the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases pool resources.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// ── Users ─────────────────────────────────────────────────────────────────────

func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (*store.User, error) {
	u := &store.User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: passwordHash,
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES ($1, $2, $3)
	`, u.ID, u.Email, u.PasswordHash)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return s.GetUserByID(ctx, u.ID)
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	u := &store.User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at, updated_at FROM users WHERE email = $1`,
		email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return u, err
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*store.User, error) {
	u := &store.User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at, updated_at FROM users WHERE id = $1`,
		id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return u, err
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID, newPasswordHash string) error {
	res, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		newPasswordHash, userID,
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	res, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ── Agent Roles ───────────────────────────────────────────────────────────────

func (s *Store) CreateRole(ctx context.Context, userID, name, description string) (*store.AgentRole, error) {
	r := &store.AgentRole{
		ID:          uuid.New().String(),
		UserID:      userID,
		Name:        name,
		Description: description,
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agent_roles (id, user_id, name, description) VALUES ($1, $2, $3, $4)`,
		r.ID, r.UserID, r.Name, r.Description,
	)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return s.GetRole(ctx, r.ID, userID)
}

func (s *Store) GetRole(ctx context.Context, id, userID string) (*store.AgentRole, error) {
	r := &store.AgentRole{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, description, created_at FROM agent_roles WHERE id = $1 AND user_id = $2`,
		id, userID,
	).Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return r, err
}

func (s *Store) ListRoles(ctx context.Context, userID string) ([]*store.AgentRole, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, description, created_at FROM agent_roles WHERE user_id = $1 ORDER BY name`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRoles(rows)
}

func (s *Store) UpdateRole(ctx context.Context, id, userID, name, description string) (*store.AgentRole, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE agent_roles SET name = $1, description = $2 WHERE id = $3 AND user_id = $4`,
		name, description, id, userID,
	)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetRole(ctx, id, userID)
}

func (s *Store) GetRoleByName(ctx context.Context, name, userID string) (*store.AgentRole, error) {
	r := &store.AgentRole{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, description, created_at FROM agent_roles WHERE name = $1 AND user_id = $2`,
		name, userID,
	).Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return r, err
}

func (s *Store) DeleteRole(ctx context.Context, id, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM agent_roles WHERE id = $1 AND user_id = $2`,
		id, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ── Policies ──────────────────────────────────────────────────────────────────

func (s *Store) CreatePolicy(ctx context.Context, userID string, p *store.PolicyRecord) (*store.PolicyRecord, error) {
	p.ID = uuid.New().String()
	p.UserID = userID
	_, err := s.pool.Exec(ctx, `
		INSERT INTO policies (id, user_id, slug, name, description, role_id, rules_yaml)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, p.ID, p.UserID, p.Slug, p.Name, p.Description, p.RoleID, p.RulesYAML)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return s.GetPolicy(ctx, p.ID, userID)
}

func (s *Store) UpdatePolicy(ctx context.Context, id, userID string, p *store.PolicyRecord) (*store.PolicyRecord, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE policies
		SET slug = $1, name = $2, description = $3, role_id = $4, rules_yaml = $5, updated_at = NOW()
		WHERE id = $6 AND user_id = $7
	`, p.Slug, p.Name, p.Description, p.RoleID, p.RulesYAML, id, userID)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetPolicy(ctx, id, userID)
}

func (s *Store) DeletePolicy(ctx context.Context, id, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM policies WHERE id = $1 AND user_id = $2`,
		id, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetPolicy(ctx context.Context, id, userID string) (*store.PolicyRecord, error) {
	p := &store.PolicyRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, slug, name, description, role_id, rules_yaml, created_at, updated_at
		FROM policies WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(&p.ID, &p.UserID, &p.Slug, &p.Name, &p.Description, &p.RoleID, &p.RulesYAML, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return p, err
}

func (s *Store) GetPolicyBySlug(ctx context.Context, slug, userID string) (*store.PolicyRecord, error) {
	p := &store.PolicyRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, slug, name, description, role_id, rules_yaml, created_at, updated_at
		FROM policies WHERE slug = $1 AND user_id = $2
	`, slug, userID).Scan(&p.ID, &p.UserID, &p.Slug, &p.Name, &p.Description, &p.RoleID, &p.RulesYAML, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return p, err
}

func (s *Store) ListPolicies(ctx context.Context, userID string, filter store.PolicyFilter) ([]*store.PolicyRecord, error) {
	query := `
		SELECT id, user_id, slug, name, description, role_id, rules_yaml, created_at, updated_at
		FROM policies WHERE user_id = $1`
	args := []any{userID}

	if filter.GlobalOnly {
		query += ` AND role_id IS NULL`
	} else if filter.RoleID != nil {
		query += ` AND role_id = $2`
		args = append(args, *filter.RoleID)
	}
	query += ` ORDER BY created_at ASC`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPolicies(rows)
}

func scanPolicies(rows pgx.Rows) ([]*store.PolicyRecord, error) {
	var policies []*store.PolicyRecord
	for rows.Next() {
		p := &store.PolicyRecord{}
		if err := rows.Scan(&p.ID, &p.UserID, &p.Slug, &p.Name, &p.Description, &p.RoleID, &p.RulesYAML, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

// ── Agents ────────────────────────────────────────────────────────────────────

func (s *Store) CreateAgent(ctx context.Context, userID, name, tokenHash string, roleID *string) (*store.Agent, error) {
	a := &store.Agent{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		TokenHash: tokenHash,
		RoleID:    roleID,
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agents (id, user_id, name, token_hash, role_id) VALUES ($1, $2, $3, $4, $5)`,
		a.ID, a.UserID, a.Name, a.TokenHash, a.RoleID,
	)
	if err != nil {
		return nil, err
	}
	return s.getAgentByID(ctx, a.ID)
}

func (s *Store) UpdateAgentRole(ctx context.Context, id, userID string, roleID *string) (*store.Agent, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE agents SET role_id = $1 WHERE id = $2 AND user_id = $3`,
		roleID, id, userID,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, store.ErrNotFound
	}
	return s.getAgentByID(ctx, id)
}

func (s *Store) GetAgentByToken(ctx context.Context, tokenHash string) (*store.Agent, error) {
	a := &store.Agent{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, token_hash, role_id, created_at FROM agents WHERE token_hash = $1`,
		tokenHash,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.RoleID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return a, err
}

func (s *Store) ListAgents(ctx context.Context, userID string) ([]*store.Agent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, token_hash, role_id, created_at FROM agents WHERE user_id = $1 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgents(rows)
}

func (s *Store) DeleteAgent(ctx context.Context, id, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM agents WHERE id = $1 AND user_id = $2`,
		id, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) getAgentByID(ctx context.Context, id string) (*store.Agent, error) {
	a := &store.Agent{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, token_hash, role_id, created_at FROM agents WHERE id = $1`,
		id,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.RoleID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return a, err
}

// ── Sessions ──────────────────────────────────────────────────────────────────

func (s *Store) CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (*store.Session, error) {
	sess := &store.Session{
		ID:        uuid.New().String(),
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) GetSession(ctx context.Context, tokenHash string) (*store.Session, error) {
	sess := &store.Session{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, token_hash, expires_at, created_at FROM sessions WHERE token_hash = $1`,
		tokenHash,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.ExpiresAt, &sess.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return sess, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE token_hash = $1`,
		tokenHash,
	)
	return err
}

// ── Service Metadata ──────────────────────────────────────────────────────────

func (s *Store) UpsertServiceMeta(ctx context.Context, userID, serviceID string, activatedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_meta (id, user_id, service_id, activated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, service_id) DO UPDATE SET
			activated_at = EXCLUDED.activated_at,
			updated_at   = NOW()
	`, uuid.New().String(), userID, serviceID, activatedAt)
	return err
}

func (s *Store) GetServiceMeta(ctx context.Context, userID, serviceID string) (*store.ServiceMeta, error) {
	m := &store.ServiceMeta{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, service_id, activated_at, updated_at FROM service_meta WHERE user_id = $1 AND service_id = $2`,
		userID, serviceID,
	).Scan(&m.ID, &m.UserID, &m.ServiceID, &m.ActivatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

func (s *Store) ListServiceMetas(ctx context.Context, userID string) ([]*store.ServiceMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, service_id, activated_at, updated_at FROM service_meta WHERE user_id = $1 ORDER BY service_id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metas []*store.ServiceMeta
	for rows.Next() {
		m := &store.ServiceMeta{}
		if err := rows.Scan(&m.ID, &m.UserID, &m.ServiceID, &m.ActivatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func (s *Store) DeleteServiceMeta(ctx context.Context, userID, serviceID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM service_meta WHERE user_id = $1 AND service_id = $2`,
		userID, serviceID,
	)
	return err
}

// ── Notification Configs ──────────────────────────────────────────────────────

func (s *Store) UpsertNotificationConfig(ctx context.Context, userID, channel string, config json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_configs (id, user_id, channel, config)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, channel) DO UPDATE SET
			config     = EXCLUDED.config,
			updated_at = NOW()
	`, uuid.New().String(), userID, channel, config)
	return err
}

func (s *Store) GetNotificationConfig(ctx context.Context, userID, channel string) (*store.NotificationConfig, error) {
	nc := &store.NotificationConfig{}
	var configJSON []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, channel, config, created_at, updated_at FROM notification_configs WHERE user_id = $1 AND channel = $2`,
		userID, channel,
	).Scan(&nc.ID, &nc.UserID, &nc.Channel, &configJSON, &nc.CreatedAt, &nc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	nc.Config = json.RawMessage(configJSON)
	return nc, nil
}

// ── Audit Log ─────────────────────────────────────────────────────────────────

func (s *Store) LogAudit(ctx context.Context, e *store.AuditEntry) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	paramsSafe := json.RawMessage("{}")
	if len(e.ParamsSafe) > 0 {
		paramsSafe = e.ParamsSafe
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (
			id, user_id, agent_id, request_id, timestamp, service, action,
			params_safe, decision, outcome, policy_id, rule_id,
			safety_flagged, safety_reason, reason, data_origin, context_src,
			duration_ms, filters_applied, error_msg
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	`, e.ID, e.UserID, e.AgentID, e.RequestID, e.Timestamp,
		e.Service, e.Action, []byte(paramsSafe), e.Decision, e.Outcome,
		e.PolicyID, e.RuleID, e.SafetyFlagged, e.SafetyReason, e.Reason,
		e.DataOrigin, e.ContextSrc, e.DurationMS, nilIfEmpty(e.FiltersApplied), e.ErrorMsg)
	return err
}

func (s *Store) UpdateAuditOutcome(ctx context.Context, id, outcome, errMsg string, durationMS int) error {
	var errMsgPtr *string
	if errMsg != "" {
		errMsgPtr = &errMsg
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE audit_log SET outcome = $1, error_msg = $2, duration_ms = $3 WHERE id = $4`,
		outcome, errMsgPtr, durationMS, id)
	return err
}

func (s *Store) GetAuditEntry(ctx context.Context, id, userID string) (*store.AuditEntry, error) {
	e := &store.AuditEntry{}
	var paramsSafe, filtersApplied []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, agent_id, request_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, error_msg
		FROM audit_log WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(
		&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.Timestamp,
		&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
		&e.PolicyID, &e.RuleID, &e.SafetyFlagged, &e.SafetyReason, &e.Reason,
		&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &e.ErrorMsg)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.ParamsSafe = json.RawMessage(paramsSafe)
	if filtersApplied != nil {
		e.FiltersApplied = json.RawMessage(filtersApplied)
	}
	return e, nil
}

func (s *Store) ListAuditEntries(ctx context.Context, userID string, filter store.AuditFilter) ([]*store.AuditEntry, int, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	where := "WHERE user_id = $1"
	args := []any{userID}
	i := 2

	if filter.Service != "" {
		where += fmt.Sprintf(" AND service = $%d", i)
		args = append(args, filter.Service)
		i++
	}
	if filter.Outcome != "" {
		where += fmt.Sprintf(" AND outcome = $%d", i)
		args = append(args, filter.Outcome)
		i++
	}
	if filter.DataOrigin != "" {
		where += fmt.Sprintf(" AND data_origin = $%d", i)
		args = append(args, filter.DataOrigin)
		i++
	}

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	dataQuery := `SELECT id, user_id, agent_id, request_id, timestamp, service, action,
		params_safe, decision, outcome, policy_id, rule_id,
		safety_flagged, safety_reason, reason, data_origin, context_src,
		duration_ms, filters_applied, error_msg
		FROM audit_log ` + where +
		fmt.Sprintf(" ORDER BY timestamp DESC LIMIT $%d OFFSET $%d", i, i+1)
	args = append(args, limit, filter.Offset)

	rows, err := s.pool.Query(ctx, dataQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries, err := scanAuditEntries(rows)
	return entries, total, err
}

// ── Pending Approvals ─────────────────────────────────────────────────────────

func (s *Store) SavePendingApproval(ctx context.Context, pa *store.PendingApproval) error {
	if pa.ID == "" {
		pa.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pending_approvals (id, user_id, request_id, audit_id, request_blob, callback_url, telegram_msg_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, pa.ID, pa.UserID, pa.RequestID, pa.AuditID, []byte(pa.RequestBlob),
		pa.CallbackURL, pa.TelegramMsgID, pa.ExpiresAt)
	return err
}

func (s *Store) GetPendingApproval(ctx context.Context, requestID string) (*store.PendingApproval, error) {
	pa := &store.PendingApproval{}
	var requestBlob []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, telegram_msg_id, expires_at, created_at
		FROM pending_approvals WHERE request_id = $1
	`, requestID).Scan(
		&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
		&pa.CallbackURL, &pa.TelegramMsgID, &pa.ExpiresAt, &pa.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pa.RequestBlob = json.RawMessage(requestBlob)
	return pa, nil
}

func (s *Store) UpdatePendingTelegramMsgID(ctx context.Context, requestID, msgID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE pending_approvals SET telegram_msg_id = $1 WHERE request_id = $2`,
		msgID, requestID)
	return err
}

func (s *Store) DeletePendingApproval(ctx context.Context, requestID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM pending_approvals WHERE request_id = $1`, requestID)
	return err
}

func (s *Store) ListExpiredPendingApprovals(ctx context.Context) ([]*store.PendingApproval, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, telegram_msg_id, expires_at, created_at
		FROM pending_approvals WHERE expires_at < NOW()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPendingApprovals(rows)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	return fmt.Sprintf("%v", err) != "" &&
		(contains(err.Error(), "duplicate key") || contains(err.Error(), "unique constraint"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func scanAuditEntries(rows pgx.Rows) ([]*store.AuditEntry, error) {
	var entries []*store.AuditEntry
	for rows.Next() {
		e := &store.AuditEntry{}
		var paramsSafe, filtersApplied []byte
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.Timestamp,
			&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
			&e.PolicyID, &e.RuleID, &e.SafetyFlagged, &e.SafetyReason, &e.Reason,
			&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &e.ErrorMsg,
		); err != nil {
			return nil, err
		}
		e.ParamsSafe = json.RawMessage(paramsSafe)
		if filtersApplied != nil {
			e.FiltersApplied = json.RawMessage(filtersApplied)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func scanPendingApprovals(rows pgx.Rows) ([]*store.PendingApproval, error) {
	var pas []*store.PendingApproval
	for rows.Next() {
		pa := &store.PendingApproval{}
		var requestBlob []byte
		if err := rows.Scan(
			&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
			&pa.CallbackURL, &pa.TelegramMsgID, &pa.ExpiresAt, &pa.CreatedAt,
		); err != nil {
			return nil, err
		}
		pa.RequestBlob = json.RawMessage(requestBlob)
		pas = append(pas, pa)
	}
	return pas, rows.Err()
}

// nilIfEmpty returns nil if the slice is empty, otherwise returns the slice.
// Used to store NULL rather than empty JSON for optional JSONB columns.
func nilIfEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func scanRoles(rows pgx.Rows) ([]*store.AgentRole, error) {
	var roles []*store.AgentRole
	for rows.Next() {
		r := &store.AgentRole{}
		if err := rows.Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

func scanAgents(rows pgx.Rows) ([]*store.Agent, error) {
	var agents []*store.Agent
	for rows.Next() {
		a := &store.Agent{}
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.RoleID, &a.CreatedAt); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}
