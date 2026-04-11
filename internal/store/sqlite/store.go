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

	"github.com/clawvisor/clawvisor/pkg/store"
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

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id != '__system__' AND email != 'admin@local'`).Scan(&n)
	return n, err
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

// ── Restrictions ──────────────────────────────────────────────────────────────

func (s *Store) CreateRestriction(ctx context.Context, r *store.Restriction) (*store.Restriction, error) {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO restrictions (id, user_id, service, action, reason)
		VALUES (?, ?, ?, ?, ?)
	`, r.ID, r.UserID, r.Service, r.Action, r.Reason)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	out := &store.Restriction{}
	var createdAt string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, user_id, service, action, reason, created_at FROM restrictions WHERE id = ?`, r.ID,
	).Scan(&out.ID, &out.UserID, &out.Service, &out.Action, &out.Reason, &createdAt)
	if err != nil {
		return nil, err
	}
	out.CreatedAt = parseTime(createdAt)
	return out, nil
}

func (s *Store) DeleteRestriction(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM restrictions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListRestrictions(ctx context.Context, userID string) ([]*store.Restriction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, service, action, reason, created_at FROM restrictions WHERE user_id = ? ORDER BY service, action`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var restrictions []*store.Restriction
	for rows.Next() {
		r := &store.Restriction{}
		var createdAt string
		if err := rows.Scan(&r.ID, &r.UserID, &r.Service, &r.Action, &r.Reason, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(createdAt)
		restrictions = append(restrictions, r)
	}
	return restrictions, rows.Err()
}

func (s *Store) MatchRestriction(ctx context.Context, userID, service, action string) (*store.Restriction, error) {
	r := &store.Restriction{}
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, service, action, reason, created_at FROM restrictions
		WHERE user_id = ? AND (service = ? OR service = '*') AND (action = ? OR action = '*')
		LIMIT 1
	`, userID, service, action).Scan(&r.ID, &r.UserID, &r.Service, &r.Action, &r.Reason, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}

// ── Agents ────────────────────────────────────────────────────────────────────

func (s *Store) CreateAgent(ctx context.Context, userID, name, tokenHash string) (*store.Agent, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, token_hash) VALUES (?, ?, ?, ?)`,
		id, userID, name, tokenHash,
	)
	if err != nil {
		return nil, err
	}
	return s.getAgentByID(ctx, id)
}

func (s *Store) CreateAgentWithOrg(ctx context.Context, userID, name, tokenHash, orgID string) (*store.Agent, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, token_hash, org_id) VALUES (?, ?, ?, ?, ?)`,
		id, userID, name, tokenHash, orgID,
	)
	if err != nil {
		return nil, err
	}
	return s.getAgentByID(ctx, id)
}

func (s *Store) GetAgentByToken(ctx context.Context, tokenHash string) (*store.Agent, error) {
	a := &store.Agent{}
	var createdAt string
	var orgID *string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, created_at, org_id FROM agents WHERE token_hash = ? AND deleted_at IS NULL`,
		tokenHash,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &createdAt, &orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = parseTime(createdAt)
	if orgID != nil {
		a.OrgID = *orgID
	}
	return a, nil
}

func (s *Store) ListAgents(ctx context.Context, userID string) ([]*store.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.user_id, a.name, a.token_hash, a.created_at, a.org_id,
		       COALESCE((SELECT COUNT(*) FROM tasks t
		                 WHERE t.agent_id = a.id
		                   AND t.status IN ('active','pending_approval','pending_scope_expansion')), 0),
		       (SELECT MAX(t.created_at) FROM tasks t WHERE t.agent_id = a.id)
		FROM agents a
		WHERE a.user_id = ? AND a.deleted_at IS NULL
		ORDER BY a.created_at DESC`,
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
		var orgID *string
		var lastTaskAt *string
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &createdAt, &orgID,
			&a.ActiveTaskCount, &lastTaskAt); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(createdAt)
		if orgID != nil {
			a.OrgID = *orgID
		}
		if lastTaskAt != nil {
			ts := parseTime(*lastTaskAt)
			a.LastTaskAt = &ts
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *Store) DeleteAgent(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET deleted_at = datetime('now') WHERE id = ? AND user_id = ? AND deleted_at IS NULL`,
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

func (s *Store) SetAgentCallbackSecret(ctx context.Context, agentID, secret string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET callback_secret = ? WHERE id = ? AND deleted_at IS NULL`,
		secret, agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetAgentCallbackSecret(ctx context.Context, agentID string) (string, error) {
	var secret *string
	err := s.db.QueryRowContext(ctx,
		`SELECT callback_secret FROM agents WHERE id = ? AND deleted_at IS NULL`, agentID,
	).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if secret == nil {
		return "", nil
	}
	return *secret, nil
}

func (s *Store) getAgentByID(ctx context.Context, id string) (*store.Agent, error) {
	a := &store.Agent{}
	var createdAt string
	var orgID *string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, created_at, org_id FROM agents WHERE id = ? AND deleted_at IS NULL`,
		id,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &createdAt, &orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = parseTime(createdAt)
	if orgID != nil {
		a.OrgID = *orgID
	}
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

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// ── Service Metadata ──────────────────────────────────────────────────────────

func (s *Store) UpsertServiceMeta(ctx context.Context, userID, serviceID, alias string, activatedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_meta (id, user_id, service_id, alias, activated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, service_id, alias) DO UPDATE SET
			activated_at = excluded.activated_at,
			updated_at   = CURRENT_TIMESTAMP
	`, uuid.New().String(), userID, serviceID, alias, activatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetServiceMeta(ctx context.Context, userID, serviceID, alias string) (*store.ServiceMeta, error) {
	m := &store.ServiceMeta{}
	var activatedAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, service_id, alias, activated_at, updated_at FROM service_meta WHERE user_id = ? AND service_id = ? AND alias = ?`,
		userID, serviceID, alias,
	).Scan(&m.ID, &m.UserID, &m.ServiceID, &m.Alias, &activatedAt, &updatedAt)
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
		`SELECT id, user_id, service_id, alias, activated_at, updated_at FROM service_meta WHERE user_id = ? ORDER BY service_id, alias`,
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
		if err := rows.Scan(&m.ID, &m.UserID, &m.ServiceID, &m.Alias, &activatedAt, &updatedAt); err != nil {
			return nil, err
		}
		m.ActivatedAt = parseTime(activatedAt)
		m.UpdatedAt = parseTime(updatedAt)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func (s *Store) DeleteServiceMeta(ctx context.Context, userID, serviceID, alias string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM service_meta WHERE user_id = ? AND service_id = ? AND alias = ?`,
		userID, serviceID, alias,
	)
	return err
}

func (s *Store) CountServiceMetasByType(ctx context.Context, userID, serviceID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM service_meta WHERE user_id = ? AND service_id = ?`,
		userID, serviceID,
	).Scan(&count)
	return count, err
}

// ── Service Configs ──────────────────────────────────────────────────────────

func (s *Store) UpsertServiceConfig(ctx context.Context, userID, serviceID, alias string, config json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO service_configs (id, user_id, service_id, alias, config)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, service_id, alias) DO UPDATE SET
			config     = excluded.config,
			updated_at = CURRENT_TIMESTAMP
	`, uuid.New().String(), userID, serviceID, alias, string(config))
	return err
}

func (s *Store) GetServiceConfig(ctx context.Context, userID, serviceID, alias string) (*store.ServiceConfig, error) {
	sc := &store.ServiceConfig{}
	var configStr, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, service_id, alias, config, created_at, updated_at FROM service_configs WHERE user_id = ? AND service_id = ? AND alias = ?`,
		userID, serviceID, alias,
	).Scan(&sc.ID, &sc.UserID, &sc.ServiceID, &sc.Alias, &configStr, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sc.Config = json.RawMessage(configStr)
	sc.CreatedAt = parseTime(createdAt)
	sc.UpdatedAt = parseTime(updatedAt)
	return sc, nil
}

func (s *Store) DeleteServiceConfig(ctx context.Context, userID, serviceID, alias string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM service_configs WHERE user_id = ? AND service_id = ? AND alias = ?`,
		userID, serviceID, alias,
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

func (s *Store) ListNotificationConfigsByChannel(ctx context.Context, channel string) ([]store.NotificationConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, channel, config, created_at, updated_at FROM notification_configs WHERE channel = ?`,
		channel,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var configs []store.NotificationConfig
	for rows.Next() {
		var nc store.NotificationConfig
		var configStr, createdAt, updatedAt string
		if err := rows.Scan(&nc.ID, &nc.UserID, &nc.Channel, &configStr, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		nc.Config = json.RawMessage(configStr)
		nc.CreatedAt = parseTime(createdAt)
		nc.UpdatedAt = parseTime(updatedAt)
		configs = append(configs, nc)
	}
	return configs, rows.Err()
}

func (s *Store) DeleteNotificationConfig(ctx context.Context, userID, channel string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM notification_configs WHERE user_id = ? AND channel = ?`,
		userID, channel,
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

// ── Gateway Request Log (append-only backup) ─────────────────────────────────

func (s *Store) LogGatewayRequest(ctx context.Context, e *store.GatewayRequestLog) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO gateway_request_log (
			audit_id, request_id, agent_id, user_id, service, action,
			task_id, reason, decision, outcome, duration_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)
	`, e.AuditID, e.RequestID, e.AgentID, e.UserID, e.Service, e.Action,
		e.TaskID, e.Reason, e.Decision, e.Outcome, e.DurationMS)
	return err
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
	var verification *string
	if len(e.Verification) > 0 {
		s := string(e.Verification)
		verification = &s
	}
	safetyFlagged := 0
	if e.SafetyFlagged {
		safetyFlagged = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (
			id, user_id, agent_id, request_id, task_id, timestamp, service, action,
			params_safe, decision, outcome, policy_id, rule_id,
			safety_flagged, safety_reason, reason, data_origin, context_src,
			duration_ms, filters_applied, verification, error_msg
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (request_id) DO NOTHING
	`, e.ID, e.UserID, e.AgentID, e.RequestID, e.TaskID, e.Timestamp.UTC().Format(time.RFC3339),
		e.Service, e.Action, paramsSafe, e.Decision, e.Outcome,
		e.PolicyID, e.RuleID, safetyFlagged, e.SafetyReason, e.Reason,
		e.DataOrigin, e.ContextSrc, e.DurationMS, filtersApplied, verification, e.ErrorMsg)
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
	var filtersApplied, verification *string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, agent_id, request_id, task_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, verification, error_msg
		FROM audit_log WHERE id = ? AND user_id = ?
	`, id, userID).Scan(
		&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.TaskID, &timestamp,
		&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
		&e.PolicyID, &e.RuleID, &safetyFlagged, &e.SafetyReason, &e.Reason,
		&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &verification, &e.ErrorMsg)
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
	if verification != nil {
		e.Verification = json.RawMessage(*verification)
	}
	return e, nil
}

func (s *Store) GetAuditEntryByRequestID(ctx context.Context, requestID, userID string) (*store.AuditEntry, error) {
	e := &store.AuditEntry{}
	var timestamp, paramsSafe string
	var safetyFlagged int
	var filtersApplied, verification *string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, agent_id, request_id, task_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, verification, error_msg
		FROM audit_log WHERE request_id = ? AND user_id = ?
	`, requestID, userID).Scan(
		&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.TaskID, &timestamp,
		&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
		&e.PolicyID, &e.RuleID, &safetyFlagged, &e.SafetyReason, &e.Reason,
		&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &verification, &e.ErrorMsg)
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
	if verification != nil {
		e.Verification = json.RawMessage(*verification)
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
	if filter.TaskID != "" {
		where += " AND task_id = ?"
		args = append(args, filter.TaskID)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	dataArgs := append(args, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, agent_id, request_id, task_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, verification, error_msg
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
		var filtersApplied, verification *string
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.TaskID, &timestamp,
			&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
			&e.PolicyID, &e.RuleID, &safetyFlagged, &e.SafetyReason, &e.Reason,
			&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &verification, &e.ErrorMsg,
		); err != nil {
			return nil, 0, err
		}
		e.Timestamp = parseTime(timestamp)
		e.SafetyFlagged = safetyFlagged != 0
		e.ParamsSafe = json.RawMessage(paramsSafe)
		if filtersApplied != nil {
			e.FiltersApplied = json.RawMessage(*filtersApplied)
		}
		if verification != nil {
			e.Verification = json.RawMessage(*verification)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// ── Audit Activity Buckets ────────────────────────────────────────────────────

func (s *Store) AuditActivityBuckets(ctx context.Context, userID string, since time.Time, bucketMinutes int) ([]store.ActivityBucket, error) {
	interval := bucketMinutes * 60
	rows, err := s.db.QueryContext(ctx, `
		SELECT datetime((CAST(strftime('%s', timestamp) AS INTEGER) / ?) * ?, 'unixepoch') AS bucket,
		       outcome, COUNT(*) AS cnt
		FROM audit_log
		WHERE user_id = ? AND timestamp >= ?
		GROUP BY bucket, outcome
		ORDER BY bucket ASC
	`, interval, interval, userID, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []store.ActivityBucket
	for rows.Next() {
		var b store.ActivityBucket
		var bucketStr string
		if err := rows.Scan(&bucketStr, &b.Outcome, &b.Count); err != nil {
			return nil, err
		}
		b.Bucket = parseTime(bucketStr)
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// ── Tasks ─────────────────────────────────────────────────────────────────────

func (s *Store) CreateTask(ctx context.Context, task *store.Task) error {
	if task.ID == "" {
		task.ID = uuid.New().String()
	}
	if task.Lifetime == "" {
		task.Lifetime = "session"
	}
	actionsJSON, err := json.Marshal(task.AuthorizedActions)
	if err != nil {
		return err
	}
	plannedCallsJSON, err := json.Marshal(task.PlannedCalls)
	if err != nil {
		return err
	}
	var pendingActionJSON *string
	if task.PendingAction != nil {
		b, err := json.Marshal(task.PendingAction)
		if err != nil {
			return err
		}
		str := string(b)
		pendingActionJSON = &str
	}
	var approvedAt, expiresAt *string
	if task.ApprovedAt != nil {
		v := task.ApprovedAt.UTC().Format(time.RFC3339)
		approvedAt = &v
	}
	if task.ExpiresAt != nil {
		v := task.ExpiresAt.UTC().Format(time.RFC3339)
		expiresAt = &v
	}
	riskDetails := string(task.RiskDetails)
	approvalRationale := string(task.ApprovalRationale)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks (id, user_id, agent_id, purpose, status, authorized_actions, planned_calls, callback_url,
			expires_in_seconds, approved_at, expires_at, pending_action, pending_reason, lifetime,
			risk_level, risk_details, approval_source, approval_rationale)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`, task.ID, task.UserID, task.AgentID, task.Purpose, task.Status,
		string(actionsJSON), string(plannedCallsJSON), task.CallbackURL, task.ExpiresInSeconds,
		approvedAt, expiresAt, pendingActionJSON, task.PendingReason, task.Lifetime,
		task.RiskLevel, riskDetails, task.ApprovalSource, approvalRationale)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (*store.Task, error) {
	t := &store.Task{}
	var actionsStr, plannedCallsStr, createdAt string
	var approvedAt, expiresAt, pendingActionStr *string
	var riskDetailsStr, approvalRationaleStr string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, agent_id, purpose, status, authorized_actions, planned_calls, callback_url,
		       created_at, approved_at, expires_at, expires_in_seconds, request_count,
		       pending_action, pending_reason, lifetime, risk_level, risk_details,
		       approval_source, approval_rationale
		FROM tasks WHERE id = ?
	`, id).Scan(&t.ID, &t.UserID, &t.AgentID, &t.Purpose, &t.Status, &actionsStr,
		&plannedCallsStr, &t.CallbackURL, &createdAt, &approvedAt, &expiresAt, &t.ExpiresInSeconds,
		&t.RequestCount, &pendingActionStr, &t.PendingReason, &t.Lifetime,
		&t.RiskLevel, &riskDetailsStr, &t.ApprovalSource, &approvalRationaleStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseTime(createdAt)
	if approvedAt != nil {
		ts := parseTime(*approvedAt)
		t.ApprovedAt = &ts
	}
	if expiresAt != nil {
		ts := parseTime(*expiresAt)
		t.ExpiresAt = &ts
	}
	if err := json.Unmarshal([]byte(actionsStr), &t.AuthorizedActions); err != nil {
		return nil, fmt.Errorf("unmarshal authorized_actions: %w", err)
	}
	if plannedCallsStr != "" {
		_ = json.Unmarshal([]byte(plannedCallsStr), &t.PlannedCalls)
	}
	if pendingActionStr != nil {
		var pa store.TaskAction
		if err := json.Unmarshal([]byte(*pendingActionStr), &pa); err == nil {
			t.PendingAction = &pa
		}
	}
	if riskDetailsStr != "" {
		t.RiskDetails = json.RawMessage(riskDetailsStr)
	}
	if approvalRationaleStr != "" {
		t.ApprovalRationale = json.RawMessage(approvalRationaleStr)
	}
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context, userID string, filter store.TaskFilter) ([]*store.Task, int, error) {
	where := "WHERE user_id = ?"
	args := []any{userID}

	if filter.Status != "" {
		where += " AND status = ?"
		args = append(args, filter.Status)
	} else if filter.ActiveOnly {
		where += " AND status IN (?, ?, ?)"
		args = append(args, "active", "pending_approval", "pending_scope_expansion")
		// Exclude session tasks that have expired but haven't been swept yet.
		where += " AND NOT (status = 'active' AND lifetime = 'session' AND expires_at IS NOT NULL AND expires_at < datetime('now'))"
	}
	if filter.Status != "" {
		where += " AND status = ?"
		args = append(args, filter.Status)
	}

	// Count total matching rows.
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, user_id, agent_id, purpose, status, authorized_actions, planned_calls, callback_url,
		       created_at, approved_at, expires_at, expires_in_seconds, request_count,
		       pending_action, pending_reason, lifetime, risk_level, risk_details,
		       approval_source, approval_rationale
		FROM tasks ` + where + ` ORDER BY created_at DESC`

	if filter.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, filter.Limit, filter.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var tasks []*store.Task
	for rows.Next() {
		t := &store.Task{}
		var actionsStr, plannedCallsStr, createdAt string
		var approvedAt, expiresAt, pendingActionStr *string
		var riskDetailsStr, approvalRationaleStr string
		if err := rows.Scan(&t.ID, &t.UserID, &t.AgentID, &t.Purpose, &t.Status, &actionsStr,
			&plannedCallsStr, &t.CallbackURL, &createdAt, &approvedAt, &expiresAt, &t.ExpiresInSeconds,
			&t.RequestCount, &pendingActionStr, &t.PendingReason, &t.Lifetime,
			&t.RiskLevel, &riskDetailsStr, &t.ApprovalSource, &approvalRationaleStr); err != nil {
			return nil, 0, err
		}
		t.CreatedAt = parseTime(createdAt)
		if approvedAt != nil {
			ts := parseTime(*approvedAt)
			t.ApprovedAt = &ts
		}
		if expiresAt != nil {
			ts := parseTime(*expiresAt)
			t.ExpiresAt = &ts
		}
		_ = json.Unmarshal([]byte(actionsStr), &t.AuthorizedActions)
		if plannedCallsStr != "" {
			_ = json.Unmarshal([]byte(plannedCallsStr), &t.PlannedCalls)
		}
		if pendingActionStr != nil {
			var pa store.TaskAction
			if err := json.Unmarshal([]byte(*pendingActionStr), &pa); err == nil {
				t.PendingAction = &pa
			}
		}
		if riskDetailsStr != "" {
			t.RiskDetails = json.RawMessage(riskDetailsStr)
		}
		if approvalRationaleStr != "" {
			t.ApprovalRationale = json.RawMessage(approvalRationaleStr)
		}
		tasks = append(tasks, t)
	}
	return tasks, total, rows.Err()
}

func (s *Store) UpdateTaskStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateTaskApproved(ctx context.Context, id string, expiresAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	exp := expiresAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = 'active', approved_at = ?, expires_at = ?
		WHERE id = ?
	`, now, exp, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateTaskActions(ctx context.Context, id string, actions []store.TaskAction, expiresAt time.Time) error {
	actionsJSON, err := json.Marshal(actions)
	if err != nil {
		return err
	}
	exp := expiresAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET authorized_actions = ?, expires_at = ?, status = 'active',
			pending_action = NULL, pending_reason = ''
		WHERE id = ?
	`, string(actionsJSON), exp, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) IncrementTaskRequestCount(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET request_count = request_count + 1 WHERE id = ?`, id)
	return err
}

func (s *Store) SetTaskPendingExpansion(ctx context.Context, id string, action *store.TaskAction, reason string) error {
	var pendingActionJSON *string
	if action != nil {
		b, _ := json.Marshal(action)
		str := string(b)
		pendingActionJSON = &str
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending_scope_expansion', pending_action = ?, pending_reason = ?
		WHERE id = ?
	`, pendingActionJSON, reason, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) RevokeTask(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'revoked' WHERE id = ? AND user_id = ? AND status = 'active'`,
		id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) RevokeTasksByAgent(ctx context.Context, agentID, userID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'revoked'
		 WHERE agent_id = ? AND user_id = ? AND status IN ('active', 'pending_approval', 'pending_scope_expansion')`,
		agentID, userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) ListExpiredTasks(ctx context.Context) ([]*store.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, agent_id, purpose, status, authorized_actions,
		       planned_calls, callback_url,
		       created_at, approved_at, expires_at, expires_in_seconds, request_count,
		       pending_action, pending_reason, lifetime, risk_level, risk_details,
		       approval_source, approval_rationale
		FROM tasks WHERE status = 'active' AND lifetime = 'session' AND expires_at < datetime('now')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*store.Task
	for rows.Next() {
		t := &store.Task{}
		var actionsStr, createdAt string
		var plannedCallsStr *string
		var approvedAt, expiresAt, pendingActionStr *string
		var riskDetailsStr, approvalRationaleStr string
		if err := rows.Scan(&t.ID, &t.UserID, &t.AgentID, &t.Purpose, &t.Status, &actionsStr,
			&plannedCallsStr, &t.CallbackURL, &createdAt, &approvedAt, &expiresAt, &t.ExpiresInSeconds,
			&t.RequestCount, &pendingActionStr, &t.PendingReason, &t.Lifetime,
			&t.RiskLevel, &riskDetailsStr, &t.ApprovalSource, &approvalRationaleStr); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(createdAt)
		if approvedAt != nil {
			ts := parseTime(*approvedAt)
			t.ApprovedAt = &ts
		}
		if expiresAt != nil {
			ts := parseTime(*expiresAt)
			t.ExpiresAt = &ts
		}
		_ = json.Unmarshal([]byte(actionsStr), &t.AuthorizedActions)
		if plannedCallsStr != nil {
			_ = json.Unmarshal([]byte(*plannedCallsStr), &t.PlannedCalls)
		}
		if pendingActionStr != nil {
			var pa store.TaskAction
			if err := json.Unmarshal([]byte(*pendingActionStr), &pa); err == nil {
				t.PendingAction = &pa
			}
		}
		if riskDetailsStr != "" {
			t.RiskDetails = json.RawMessage(riskDetailsStr)
		}
		if approvalRationaleStr != "" {
			t.ApprovalRationale = json.RawMessage(approvalRationaleStr)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ── Pending Approvals ─────────────────────────────────────────────────────────

func (s *Store) SavePendingApproval(ctx context.Context, pa *store.PendingApproval) error {
	if pa.ID == "" {
		pa.ID = uuid.New().String()
	}
	if pa.Status == "" {
		pa.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_approvals (id, user_id, request_id, audit_id, request_blob, callback_url, status, expires_at)
		VALUES (?,?,?,?,?,?,?,?)
	`, pa.ID, pa.UserID, pa.RequestID, pa.AuditID, string(pa.RequestBlob),
		pa.CallbackURL, pa.Status, pa.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetPendingApproval(ctx context.Context, requestID string) (*store.PendingApproval, error) {
	pa := &store.PendingApproval{}
	var requestBlob, expiresAt, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, status, expires_at, created_at
		FROM pending_approvals WHERE request_id = ?
	`, requestID).Scan(
		&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
		&pa.CallbackURL, &pa.Status, &expiresAt, &createdAt)
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

func (s *Store) DeletePendingApproval(ctx context.Context, requestID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_approvals WHERE request_id = ?`, requestID)
	return err
}

func (s *Store) ListPendingApprovals(ctx context.Context, userID string) ([]*store.PendingApproval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, status, expires_at, created_at
		FROM pending_approvals WHERE user_id = ? AND status = 'pending' AND expires_at > datetime('now') ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLitePendingApprovals(rows)
}

func (s *Store) ListExpiredPendingApprovals(ctx context.Context) ([]*store.PendingApproval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, status, expires_at, created_at
		FROM pending_approvals WHERE status = 'pending' AND expires_at < datetime('now')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLitePendingApprovals(rows)
}

func scanSQLitePendingApprovals(rows *sql.Rows) ([]*store.PendingApproval, error) {
	var pas []*store.PendingApproval
	for rows.Next() {
		pa := &store.PendingApproval{}
		var requestBlob, expiresAt, createdAt string
		if err := rows.Scan(
			&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
			&pa.CallbackURL, &pa.Status, &expiresAt, &createdAt,
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

func (s *Store) UpdatePendingApprovalStatus(ctx context.Context, requestID, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pending_approvals SET status = ? WHERE request_id = ?`, status, requestID)
	return err
}

func (s *Store) ClaimPendingApprovalForExecution(ctx context.Context, requestID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_approvals SET status = 'executing' WHERE request_id = ? AND status = 'approved'`, requestID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ── Notification Messages ──────────────────────────────────────────────────────

func (s *Store) SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_messages (target_type, target_id, channel, message_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (target_type, target_id, channel) DO UPDATE SET
			message_id = excluded.message_id
	`, targetType, targetID, channel, messageID)
	return err
}

func (s *Store) GetNotificationMessage(ctx context.Context, targetType, targetID, channel string) (string, error) {
	var messageID string
	err := s.db.QueryRowContext(ctx, `
		SELECT message_id FROM notification_messages
		WHERE target_type = ? AND target_id = ? AND channel = ?
	`, targetType, targetID, channel).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return messageID, nil
}

// ── OAuth ────────────────────────────────────────────────────────────────────

func (s *Store) CreateOAuthClient(ctx context.Context, client *store.OAuthClient) error {
	urisJSON, _ := json.Marshal(client.RedirectURIs)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_clients (id, client_name, redirect_uris) VALUES (?, ?, ?)`,
		client.ID, client.ClientName, string(urisJSON),
	)
	return err
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (*store.OAuthClient, error) {
	c := &store.OAuthClient{}
	var uris, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, client_name, redirect_uris, created_at FROM oauth_clients WHERE id = ?`,
		clientID,
	).Scan(&c.ID, &c.ClientName, &uris, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = parseTime(createdAt)
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("parsing redirect_uris: %w", err)
	}
	return c, nil
}

func (s *Store) SaveAuthorizationCode(ctx context.Context, code *store.OAuthAuthorizationCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_authorization_codes (code_hash, client_id, user_id, daemon_id, redirect_uri, code_challenge, scope, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		code.CodeHash, code.ClientID, code.UserID, code.DaemonID, code.RedirectURI, code.CodeChallenge, code.Scope,
		code.ExpiresAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (s *Store) ConsumeAuthorizationCode(ctx context.Context, codeHash string) (*store.OAuthAuthorizationCode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	c := &store.OAuthAuthorizationCode{}
	var expiresAt, createdAt string
	err = tx.QueryRowContext(ctx,
		`SELECT code_hash, client_id, user_id, daemon_id, redirect_uri, code_challenge, scope, expires_at, created_at
		 FROM oauth_authorization_codes WHERE code_hash = ?`,
		codeHash,
	).Scan(&c.CodeHash, &c.ClientID, &c.UserID, &c.DaemonID, &c.RedirectURI, &c.CodeChallenge, &c.Scope, &expiresAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_authorization_codes WHERE code_hash = ?`, codeHash); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	c.ExpiresAt = parseTime(expiresAt)
	c.CreatedAt = parseTime(createdAt)
	return c, nil
}

// ── Chain Facts ───────────────────────────────────────────────────────────────

func (s *Store) SaveChainFacts(ctx context.Context, facts []*store.ChainFact) error {
	for _, f := range facts {
		if f.ID == "" {
			f.ID = uuid.New().String()
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO chain_facts (id, task_id, session_id, audit_id, service, action, fact_type, fact_value)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, f.ID, f.TaskID, f.SessionID, f.AuditID, f.Service, f.Action, f.FactType, f.FactValue)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListChainFacts(ctx context.Context, taskID, sessionID string, limit int) ([]*store.ChainFact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, session_id, audit_id, service, action, fact_type, fact_value, created_at
		FROM chain_facts WHERE task_id = ? AND session_id = ? ORDER BY created_at ASC LIMIT ?
	`, taskID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var facts []*store.ChainFact
	for rows.Next() {
		f := &store.ChainFact{}
		var createdAt string
		if err := rows.Scan(&f.ID, &f.TaskID, &f.SessionID, &f.AuditID,
			&f.Service, &f.Action, &f.FactType, &f.FactValue, &createdAt); err != nil {
			return nil, err
		}
		f.CreatedAt = parseTime(createdAt)
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

func (s *Store) ChainFactValueExists(ctx context.Context, taskID, sessionID, value string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM chain_facts WHERE task_id = ? AND session_id = ? AND fact_value = ? LIMIT 1
	`, taskID, sessionID, value).Scan(&exists)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) DeleteChainFactsByTask(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chain_facts WHERE task_id = ?`, taskID)
	return err
}

// ── Connection Requests ───────────────────────────────────────────────────────

func (s *Store) CreateConnectionRequest(ctx context.Context, req *store.ConnectionRequest) error {
	if req.ID == "" {
		req.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO connection_requests (id, user_id, name, description, callback_url, status, ip_address, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, req.ID, req.UserID, req.Name, req.Description, req.CallbackURL, req.Status, req.IPAddress,
		req.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetConnectionRequest(ctx context.Context, id string) (*store.ConnectionRequest, error) {
	r := &store.ConnectionRequest{}
	var createdAt, expiresAt string
	var agentID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, description, callback_url, status, agent_id, ip_address, created_at, expires_at
		FROM connection_requests WHERE id = ?
	`, id).Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &r.CallbackURL, &r.Status,
		&agentID, &r.IPAddress, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if agentID.Valid {
		r.AgentID = agentID.String
	}
	r.CreatedAt = parseTime(createdAt)
	r.ExpiresAt = parseTime(expiresAt)
	return r, nil
}

func (s *Store) ListPendingConnectionRequests(ctx context.Context, userID string) ([]*store.ConnectionRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, description, callback_url, status, agent_id, ip_address, created_at, expires_at
		FROM connection_requests WHERE user_id = ? AND status = 'pending' ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*store.ConnectionRequest
	for rows.Next() {
		r := &store.ConnectionRequest{}
		var createdAt, expiresAt string
		var agentID sql.NullString
		if err := rows.Scan(&r.ID, &r.UserID, &r.Name, &r.Description, &r.CallbackURL, &r.Status,
			&agentID, &r.IPAddress, &createdAt, &expiresAt); err != nil {
			return nil, err
		}
		if agentID.Valid {
			r.AgentID = agentID.String
		}
		r.CreatedAt = parseTime(createdAt)
		r.ExpiresAt = parseTime(expiresAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateConnectionRequestStatus(ctx context.Context, id, status, agentID string) error {
	var res sql.Result
	var err error
	if agentID != "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE connection_requests SET status = ?, agent_id = ? WHERE id = ?`,
			status, agentID, id)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE connection_requests SET status = ? WHERE id = ?`,
			status, id)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteExpiredConnectionRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM connection_requests WHERE status = 'pending' AND expires_at < datetime('now')`)
	return err
}

func (s *Store) CountPendingConnectionRequests(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connection_requests WHERE status = 'pending'`).Scan(&count)
	return count, err
}

// ── Paired Devices ────────────────────────────────────────────────────────────

func (s *Store) CreatePairedDevice(ctx context.Context, d *store.PairedDevice) error {
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO paired_devices (id, user_id, device_name, device_token, device_hmac_key, push_to_start_token)
		VALUES (?, ?, ?, ?, ?, ?)
	`, d.ID, d.UserID, d.DeviceName, d.DeviceToken, d.DeviceHMACKey, d.PushToStartToken)
	return err
}

func (s *Store) GetPairedDevice(ctx context.Context, id string) (*store.PairedDevice, error) {
	d := &store.PairedDevice{}
	var pairedAt, lastSeenAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, device_name, device_token, device_hmac_key, push_to_start_token, paired_at, last_seen_at
		FROM paired_devices WHERE id = ?
	`, id).Scan(&d.ID, &d.UserID, &d.DeviceName, &d.DeviceToken, &d.DeviceHMACKey, &d.PushToStartToken, &pairedAt, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.PairedAt = parseTime(pairedAt)
	d.LastSeenAt = parseTime(lastSeenAt)
	return d, nil
}

func (s *Store) ListPairedDevices(ctx context.Context, userID string) ([]*store.PairedDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, device_name, device_token, device_hmac_key, push_to_start_token, paired_at, last_seen_at
		FROM paired_devices WHERE user_id = ? ORDER BY paired_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []*store.PairedDevice
	for rows.Next() {
		d := &store.PairedDevice{}
		var pairedAt, lastSeenAt string
		if err := rows.Scan(&d.ID, &d.UserID, &d.DeviceName, &d.DeviceToken, &d.DeviceHMACKey, &d.PushToStartToken,
			&pairedAt, &lastSeenAt); err != nil {
			return nil, err
		}
		d.PairedAt = parseTime(pairedAt)
		d.LastSeenAt = parseTime(lastSeenAt)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) ListPairedDevicesByDeviceToken(ctx context.Context, deviceToken string) ([]*store.PairedDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, device_name, device_token, device_hmac_key, push_to_start_token, paired_at, last_seen_at
		FROM paired_devices WHERE device_token = ? ORDER BY paired_at DESC
	`, deviceToken)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []*store.PairedDevice
	for rows.Next() {
		d := &store.PairedDevice{}
		var pairedAt, lastSeenAt string
		if err := rows.Scan(&d.ID, &d.UserID, &d.DeviceName, &d.DeviceToken, &d.DeviceHMACKey, &d.PushToStartToken,
			&pairedAt, &lastSeenAt); err != nil {
			return nil, err
		}
		d.PairedAt = parseTime(pairedAt)
		d.LastSeenAt = parseTime(lastSeenAt)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) DeletePairedDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM paired_devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UpdatePairedDeviceLastSeen(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE paired_devices SET last_seen_at = datetime('now') WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UpdatePairedDevicePushToStartToken(ctx context.Context, id, token string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE paired_devices SET push_to_start_token = ? WHERE id = ?`, token, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ── MCP Sessions ─────────────────────────────────────────────────────────────

func (s *Store) CreateMCPSession(ctx context.Context, id string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_sessions (id, expires_at) VALUES (?, ?)`,
		id, expiresAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (s *Store) MCPSessionValid(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM mcp_sessions WHERE id = ? AND expires_at > ?`,
		id, time.Now().UTC().Format(time.RFC3339),
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) CleanupMCPSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM mcp_sessions WHERE expires_at <= ?`,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
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

// TelemetryCounts returns aggregate, anonymous usage data for telemetry.
func (s *Store) TelemetryCounts(ctx context.Context) (*store.TelemetryCounts, error) {
	c := &store.TelemetryCounts{
		RequestsByService: make(map[string]int),
	}

	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE deleted_at IS NULL").Scan(&c.Agents); err != nil {
		return nil, fmt.Errorf("counting agents: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, "SELECT service, COUNT(*) FROM audit_log GROUP BY service")
	if err != nil {
		return nil, fmt.Errorf("counting requests by service: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var svc string
		var count int
		if err := rows.Scan(&svc, &count); err != nil {
			return nil, err
		}
		c.RequestsByService[svc] = count
	}
	return c, rows.Err()
}

// ── Agent-group pairings ──────────────────────────────────────────────────────

func (s *Store) CreateAgentGroupPairing(ctx context.Context, userID, agentID, groupChatID string) error {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_group_pairings (id, user_id, agent_id, group_chat_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (agent_id) DO UPDATE SET group_chat_id = excluded.group_chat_id, user_id = excluded.user_id
	`, id, userID, agentID, groupChatID)
	return err
}

func (s *Store) GetAgentGroupChatID(ctx context.Context, agentID string) (string, error) {
	var groupChatID string
	err := s.db.QueryRowContext(ctx, `SELECT group_chat_id FROM agent_group_pairings WHERE agent_id = ?`, agentID).Scan(&groupChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return groupChatID, err
}

func (s *Store) ListAgentIDsByGroup(ctx context.Context, groupChatID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id FROM agent_group_pairings WHERE group_chat_id = ?`, groupChatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) DeleteAgentGroupPairing(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_group_pairings WHERE agent_id = ?`, agentID)
	return err
}

func (s *Store) DeleteAgentGroupPairingsByGroup(ctx context.Context, groupChatID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_group_pairings WHERE group_chat_id = ?`, groupChatID)
	return err
}

// ── Telegram Groups ─────────────────────────────────────────────────────────

func (s *Store) CreateTelegramGroup(ctx context.Context, userID, groupChatID, title string) (*store.TelegramGroup, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO telegram_groups (id, user_id, group_chat_id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, userID, groupChatID, title, now, now)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return &store.TelegramGroup{
		ID:          id,
		UserID:      userID,
		GroupChatID: groupChatID,
		Title:       title,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

func (s *Store) GetTelegramGroup(ctx context.Context, userID, groupChatID string) (*store.TelegramGroup, error) {
	var g store.TelegramGroup
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, group_chat_id, title, auto_approval_enabled, auto_approval_notify, created_at, updated_at
		FROM telegram_groups WHERE user_id = ? AND group_chat_id = ?
	`, userID, groupChatID).Scan(&g.ID, &g.UserID, &g.GroupChatID, &g.Title, &g.AutoApprovalEnabled, &g.AutoApprovalNotify, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	g.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &g, nil
}

func (s *Store) ListTelegramGroups(ctx context.Context, userID string) ([]*store.TelegramGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, group_chat_id, title, auto_approval_enabled, auto_approval_notify, created_at, updated_at
		FROM telegram_groups WHERE user_id = ? ORDER BY created_at
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []*store.TelegramGroup
	for rows.Next() {
		var g store.TelegramGroup
		var createdAt, updatedAt string
		if err := rows.Scan(&g.ID, &g.UserID, &g.GroupChatID, &g.Title, &g.AutoApprovalEnabled, &g.AutoApprovalNotify, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		g.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		g.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		groups = append(groups, &g)
	}
	return groups, rows.Err()
}

func (s *Store) ListAllTelegramGroups(ctx context.Context) ([]*store.TelegramGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, group_chat_id, title, auto_approval_enabled, auto_approval_notify, created_at, updated_at
		FROM telegram_groups ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []*store.TelegramGroup
	for rows.Next() {
		var g store.TelegramGroup
		var createdAt, updatedAt string
		if err := rows.Scan(&g.ID, &g.UserID, &g.GroupChatID, &g.Title, &g.AutoApprovalEnabled, &g.AutoApprovalNotify, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		g.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		g.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		groups = append(groups, &g)
	}
	return groups, rows.Err()
}

func (s *Store) UpdateTelegramGroupAutoApproval(ctx context.Context, userID, groupChatID string, enabled bool, notify *bool) error {
	if notify != nil {
		_, err := s.db.ExecContext(ctx, `
			UPDATE telegram_groups SET auto_approval_enabled = ?, auto_approval_notify = ?, updated_at = datetime('now')
			WHERE user_id = ? AND group_chat_id = ?
		`, enabled, *notify, userID, groupChatID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE telegram_groups SET auto_approval_enabled = ?, updated_at = datetime('now')
		WHERE user_id = ? AND group_chat_id = ?
	`, enabled, userID, groupChatID)
	return err
}

func (s *Store) DeleteTelegramGroup(ctx context.Context, userID, groupChatID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM telegram_groups WHERE user_id = ? AND group_chat_id = ?`, userID, groupChatID)
	return err
}

// ── Generated Adapters ─────────────────────────────────────────────────────────

func (s *Store) SaveGeneratedAdapter(ctx context.Context, userID, serviceID, yamlContent string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO generated_adapters (user_id, service_id, yaml_content)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id, service_id) DO UPDATE SET
			yaml_content = excluded.yaml_content,
			updated_at = CURRENT_TIMESTAMP
	`, userID, serviceID, yamlContent)
	return err
}

func (s *Store) ListGeneratedAdapters(ctx context.Context, userID string) ([]*store.GeneratedAdapter, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, service_id, yaml_content, created_at, updated_at
		 FROM generated_adapters WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.GeneratedAdapter
	for rows.Next() {
		a := &store.GeneratedAdapter{}
		var createdAt, updatedAt string
		if err := rows.Scan(&a.UserID, &a.ServiceID, &a.YAMLContent, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(createdAt)
		a.UpdatedAt = parseTime(updatedAt)
		out = append(out, a)
	}
	return out, rows.Err()
}


func (s *Store) DeleteGeneratedAdapter(ctx context.Context, userID, serviceID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM generated_adapters WHERE user_id = ? AND service_id = ?`,
		userID, serviceID)
	return err
}

// Ensure Store implements store.Store at compile time.
var _ store.Store = (*Store)(nil)

// unused import guard
var _ = fmt.Sprintf
