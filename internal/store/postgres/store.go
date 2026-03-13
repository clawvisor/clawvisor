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

	"github.com/clawvisor/clawvisor/pkg/store"
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

// ── Restrictions ──────────────────────────────────────────────────────────────

func (s *Store) CreateRestriction(ctx context.Context, r *store.Restriction) (*store.Restriction, error) {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restrictions (id, user_id, service, action, reason)
		VALUES ($1, $2, $3, $4, $5)
	`, r.ID, r.UserID, r.Service, r.Action, r.Reason)
	if err != nil {
		if isDuplicate(err) {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	out := &store.Restriction{}
	err = s.pool.QueryRow(ctx,
		`SELECT id, user_id, service, action, reason, created_at FROM restrictions WHERE id = $1`, r.ID,
	).Scan(&out.ID, &out.UserID, &out.Service, &out.Action, &out.Reason, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) DeleteRestriction(ctx context.Context, id, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM restrictions WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListRestrictions(ctx context.Context, userID string) ([]*store.Restriction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, service, action, reason, created_at FROM restrictions WHERE user_id = $1 ORDER BY service, action`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var restrictions []*store.Restriction
	for rows.Next() {
		r := &store.Restriction{}
		if err := rows.Scan(&r.ID, &r.UserID, &r.Service, &r.Action, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		restrictions = append(restrictions, r)
	}
	return restrictions, rows.Err()
}

func (s *Store) MatchRestriction(ctx context.Context, userID, service, action string) (*store.Restriction, error) {
	r := &store.Restriction{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, service, action, reason, created_at FROM restrictions
		WHERE user_id = $1 AND (service = $2 OR service = '*') AND (action = $3 OR action = '*')
		LIMIT 1
	`, userID, service, action).Scan(&r.ID, &r.UserID, &r.Service, &r.Action, &r.Reason, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ── Agents ────────────────────────────────────────────────────────────────────

func (s *Store) CreateAgent(ctx context.Context, userID, name, tokenHash string) (*store.Agent, error) {
	a := &store.Agent{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		TokenHash: tokenHash,
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agents (id, user_id, name, token_hash) VALUES ($1, $2, $3, $4)`,
		a.ID, a.UserID, a.Name, a.TokenHash,
	)
	if err != nil {
		return nil, err
	}
	return s.getAgentByID(ctx, a.ID)
}

func (s *Store) GetAgentByToken(ctx context.Context, tokenHash string) (*store.Agent, error) {
	a := &store.Agent{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, token_hash, created_at FROM agents WHERE token_hash = $1`,
		tokenHash,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return a, err
}

func (s *Store) ListAgents(ctx context.Context, userID string) ([]*store.Agent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, token_hash, created_at FROM agents WHERE user_id = $1 ORDER BY created_at DESC`,
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

func (s *Store) SetAgentCallbackSecret(ctx context.Context, agentID, secret string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE agents SET callback_secret = $1 WHERE id = $2`,
		secret, agentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) GetAgentCallbackSecret(ctx context.Context, agentID string) (string, error) {
	var secret *string
	err := s.pool.QueryRow(ctx,
		`SELECT callback_secret FROM agents WHERE id = $1`, agentID,
	).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
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
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, token_hash, created_at FROM agents WHERE id = $1`,
		id,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.CreatedAt)
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

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	return err
}

// ── Service Metadata ──────────────────────────────────────────────────────────

func (s *Store) UpsertServiceMeta(ctx context.Context, userID, serviceID, alias string, activatedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_meta (id, user_id, service_id, alias, activated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, service_id, alias) DO UPDATE SET
			activated_at = EXCLUDED.activated_at,
			updated_at   = NOW()
	`, uuid.New().String(), userID, serviceID, alias, activatedAt)
	return err
}

func (s *Store) GetServiceMeta(ctx context.Context, userID, serviceID, alias string) (*store.ServiceMeta, error) {
	m := &store.ServiceMeta{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, service_id, alias, activated_at, updated_at FROM service_meta WHERE user_id = $1 AND service_id = $2 AND alias = $3`,
		userID, serviceID, alias,
	).Scan(&m.ID, &m.UserID, &m.ServiceID, &m.Alias, &m.ActivatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

func (s *Store) ListServiceMetas(ctx context.Context, userID string) ([]*store.ServiceMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, service_id, alias, activated_at, updated_at FROM service_meta WHERE user_id = $1 ORDER BY service_id, alias`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metas []*store.ServiceMeta
	for rows.Next() {
		m := &store.ServiceMeta{}
		if err := rows.Scan(&m.ID, &m.UserID, &m.ServiceID, &m.Alias, &m.ActivatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func (s *Store) DeleteServiceMeta(ctx context.Context, userID, serviceID, alias string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM service_meta WHERE user_id = $1 AND service_id = $2 AND alias = $3`,
		userID, serviceID, alias,
	)
	return err
}

func (s *Store) CountServiceMetasByType(ctx context.Context, userID, serviceID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM service_meta WHERE user_id = $1 AND service_id = $2`,
		userID, serviceID,
	).Scan(&count)
	return count, err
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

func (s *Store) DeleteNotificationConfig(ctx context.Context, userID, channel string) error {
	res, err := s.pool.Exec(ctx,
		`DELETE FROM notification_configs WHERE user_id = $1 AND channel = $2`,
		userID, channel,
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
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
			id, user_id, agent_id, request_id, task_id, timestamp, service, action,
			params_safe, decision, outcome, policy_id, rule_id,
			safety_flagged, safety_reason, reason, data_origin, context_src,
			duration_ms, filters_applied, verification, error_msg
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
	`, e.ID, e.UserID, e.AgentID, e.RequestID, e.TaskID, e.Timestamp,
		e.Service, e.Action, []byte(paramsSafe), e.Decision, e.Outcome,
		e.PolicyID, e.RuleID, e.SafetyFlagged, e.SafetyReason, e.Reason,
		e.DataOrigin, e.ContextSrc, e.DurationMS, nilIfEmpty(e.FiltersApplied),
		nilIfEmpty(e.Verification), e.ErrorMsg)
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
	var paramsSafe, filtersApplied, verification []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, agent_id, request_id, task_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, verification, error_msg
		FROM audit_log WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(
		&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.TaskID, &e.Timestamp,
		&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
		&e.PolicyID, &e.RuleID, &e.SafetyFlagged, &e.SafetyReason, &e.Reason,
		&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &verification, &e.ErrorMsg)
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
	if verification != nil {
		e.Verification = json.RawMessage(verification)
	}
	return e, nil
}

func (s *Store) GetAuditEntryByRequestID(ctx context.Context, requestID, userID string) (*store.AuditEntry, error) {
	e := &store.AuditEntry{}
	var paramsSafe, filtersApplied, verification []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, agent_id, request_id, task_id, timestamp, service, action,
		       params_safe, decision, outcome, policy_id, rule_id,
		       safety_flagged, safety_reason, reason, data_origin, context_src,
		       duration_ms, filters_applied, verification, error_msg
		FROM audit_log WHERE request_id = $1 AND user_id = $2
	`, requestID, userID).Scan(
		&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.TaskID, &e.Timestamp,
		&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
		&e.PolicyID, &e.RuleID, &e.SafetyFlagged, &e.SafetyReason, &e.Reason,
		&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &verification, &e.ErrorMsg)
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
	if verification != nil {
		e.Verification = json.RawMessage(verification)
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
	if filter.TaskID != "" {
		where += fmt.Sprintf(" AND task_id = $%d", i)
		args = append(args, filter.TaskID)
		i++
	}

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	dataQuery := `SELECT id, user_id, agent_id, request_id, task_id, timestamp, service, action,
		params_safe, decision, outcome, policy_id, rule_id,
		safety_flagged, safety_reason, reason, data_origin, context_src,
		duration_ms, filters_applied, verification, error_msg
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

// ── Audit Activity Buckets ────────────────────────────────────────────────────

func (s *Store) AuditActivityBuckets(ctx context.Context, userID string, since time.Time, bucketMinutes int) ([]store.ActivityBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc('minute', timestamp)
		         - (EXTRACT(minute FROM timestamp)::int % $3) * interval '1 minute' AS bucket,
		       outcome, COUNT(*) AS cnt
		FROM audit_log
		WHERE user_id = $1 AND timestamp >= $2
		GROUP BY bucket, outcome
		ORDER BY bucket ASC
	`, userID, since, bucketMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []store.ActivityBucket
	for rows.Next() {
		var b store.ActivityBucket
		if err := rows.Scan(&b.Bucket, &b.Outcome, &b.Count); err != nil {
			return nil, err
		}
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
	var pendingActionJSON []byte
	if task.PendingAction != nil {
		pendingActionJSON, _ = json.Marshal(task.PendingAction)
	}
	var riskDetails []byte
	if task.RiskDetails != nil {
		riskDetails = []byte(task.RiskDetails)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO tasks (id, user_id, agent_id, purpose, status, authorized_actions, callback_url,
			expires_in_seconds, approved_at, expires_at, pending_action, pending_reason, lifetime,
			risk_level, risk_details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, task.ID, task.UserID, task.AgentID, task.Purpose, task.Status,
		actionsJSON, task.CallbackURL, task.ExpiresInSeconds,
		task.ApprovedAt, task.ExpiresAt,
		nilIfEmpty(pendingActionJSON), task.PendingReason, task.Lifetime,
		task.RiskLevel, string(riskDetails))
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (*store.Task, error) {
	t := &store.Task{}
	var actionsJSON, pendingActionJSON []byte
	var riskDetailsStr string
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, agent_id, purpose, status, authorized_actions, callback_url,
		       created_at, approved_at, expires_at, expires_in_seconds, request_count,
		       pending_action, pending_reason, lifetime, risk_level, risk_details
		FROM tasks WHERE id = $1
	`, id).Scan(&t.ID, &t.UserID, &t.AgentID, &t.Purpose, &t.Status, &actionsJSON,
		&t.CallbackURL, &t.CreatedAt, &t.ApprovedAt, &t.ExpiresAt, &t.ExpiresInSeconds,
		&t.RequestCount, &pendingActionJSON, &t.PendingReason, &t.Lifetime,
		&t.RiskLevel, &riskDetailsStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(actionsJSON, &t.AuthorizedActions); err != nil {
		return nil, fmt.Errorf("unmarshal authorized_actions: %w", err)
	}
	if pendingActionJSON != nil {
		var pa store.TaskAction
		if err := json.Unmarshal(pendingActionJSON, &pa); err == nil {
			t.PendingAction = &pa
		}
	}
	if riskDetailsStr != "" {
		t.RiskDetails = json.RawMessage(riskDetailsStr)
	}
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context, userID string, filter store.TaskFilter) ([]*store.Task, int, error) {
	where := "WHERE user_id = $1"
	args := []any{userID}
	argIdx := 2

	if filter.ActiveOnly {
		where += fmt.Sprintf(" AND status IN ($%d, $%d, $%d)", argIdx, argIdx+1, argIdx+2)
		args = append(args, "active", "pending_approval", "pending_scope_expansion")
		argIdx += 3
	}

	// Count total matching rows.
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM tasks "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, user_id, agent_id, purpose, status, authorized_actions, callback_url,
		       created_at, approved_at, expires_at, expires_in_seconds, request_count,
		       pending_action, pending_reason, lifetime, risk_level, risk_details
		FROM tasks ` + where + ` ORDER BY created_at DESC`

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
		args = append(args, filter.Limit, filter.Offset)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, 0, err
	}
	return tasks, total, nil
}

func (s *Store) UpdateTaskStatus(ctx context.Context, id, status string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = $1 WHERE id = $2`, status, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateTaskApproved(ctx context.Context, id string, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks SET status = 'active', approved_at = NOW(), expires_at = $1
		WHERE id = $2
	`, expiresAt, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateTaskActions(ctx context.Context, id string, actions []store.TaskAction, expiresAt time.Time) error {
	actionsJSON, err := json.Marshal(actions)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks SET authorized_actions = $1, expires_at = $2, status = 'active',
			pending_action = NULL, pending_reason = ''
		WHERE id = $3
	`, actionsJSON, expiresAt, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) IncrementTaskRequestCount(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tasks SET request_count = request_count + 1 WHERE id = $1`, id)
	return err
}

func (s *Store) SetTaskPendingExpansion(ctx context.Context, id string, action *store.TaskAction, reason string) error {
	var pendingActionJSON []byte
	if action != nil {
		pendingActionJSON, _ = json.Marshal(action)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks SET status = 'pending_scope_expansion', pending_action = $1, pending_reason = $2
		WHERE id = $3
	`, nilIfEmpty(pendingActionJSON), reason, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) RevokeTask(ctx context.Context, id, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = 'revoked' WHERE id = $1 AND user_id = $2 AND status = 'active'`,
		id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListExpiredTasks(ctx context.Context) ([]*store.Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, agent_id, purpose, status, authorized_actions, callback_url,
		       created_at, approved_at, expires_at, expires_in_seconds, request_count,
		       pending_action, pending_reason, lifetime, risk_level, risk_details
		FROM tasks WHERE status = 'active' AND lifetime = 'session' AND expires_at < NOW()
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func scanTasks(rows pgx.Rows) ([]*store.Task, error) {
	var tasks []*store.Task
	for rows.Next() {
		t := &store.Task{}
		var actionsJSON, pendingActionJSON []byte
		var riskDetailsStr string
		if err := rows.Scan(&t.ID, &t.UserID, &t.AgentID, &t.Purpose, &t.Status, &actionsJSON,
			&t.CallbackURL, &t.CreatedAt, &t.ApprovedAt, &t.ExpiresAt, &t.ExpiresInSeconds,
			&t.RequestCount, &pendingActionJSON, &t.PendingReason, &t.Lifetime,
			&t.RiskLevel, &riskDetailsStr); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(actionsJSON, &t.AuthorizedActions)
		if pendingActionJSON != nil {
			var pa store.TaskAction
			if err := json.Unmarshal(pendingActionJSON, &pa); err == nil {
				t.PendingAction = &pa
			}
		}
		if riskDetailsStr != "" {
			t.RiskDetails = json.RawMessage(riskDetailsStr)
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
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pending_approvals (id, user_id, request_id, audit_id, request_blob, callback_url, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, pa.ID, pa.UserID, pa.RequestID, pa.AuditID, []byte(pa.RequestBlob),
		pa.CallbackURL, pa.ExpiresAt)
	return err
}

func (s *Store) GetPendingApproval(ctx context.Context, requestID string) (*store.PendingApproval, error) {
	pa := &store.PendingApproval{}
	var requestBlob []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, expires_at, created_at
		FROM pending_approvals WHERE request_id = $1
	`, requestID).Scan(
		&pa.ID, &pa.UserID, &pa.RequestID, &pa.AuditID, &requestBlob,
		&pa.CallbackURL, &pa.ExpiresAt, &pa.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pa.RequestBlob = json.RawMessage(requestBlob)
	return pa, nil
}

func (s *Store) DeletePendingApproval(ctx context.Context, requestID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM pending_approvals WHERE request_id = $1`, requestID)
	return err
}

func (s *Store) ListPendingApprovals(ctx context.Context, userID string) ([]*store.PendingApproval, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, expires_at, created_at
		FROM pending_approvals WHERE user_id = $1 AND expires_at > NOW() ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPendingApprovals(rows)
}

func (s *Store) ListExpiredPendingApprovals(ctx context.Context) ([]*store.PendingApproval, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, request_id, audit_id, request_blob, callback_url, expires_at, created_at
		FROM pending_approvals WHERE expires_at < NOW()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPendingApprovals(rows)
}

// ── Notification Messages ──────────────────────────────────────────────────────

func (s *Store) SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_messages (target_type, target_id, channel, message_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (target_type, target_id, channel) DO UPDATE SET
			message_id = EXCLUDED.message_id
	`, targetType, targetID, channel, messageID)
	return err
}

func (s *Store) GetNotificationMessage(ctx context.Context, targetType, targetID, channel string) (string, error) {
	var messageID string
	err := s.pool.QueryRow(ctx, `
		SELECT message_id FROM notification_messages
		WHERE target_type = $1 AND target_id = $2 AND channel = $3
	`, targetType, targetID, channel).Scan(&messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return messageID, nil
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
		var paramsSafe, filtersApplied, verification []byte
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.AgentID, &e.RequestID, &e.TaskID, &e.Timestamp,
			&e.Service, &e.Action, &paramsSafe, &e.Decision, &e.Outcome,
			&e.PolicyID, &e.RuleID, &e.SafetyFlagged, &e.SafetyReason, &e.Reason,
			&e.DataOrigin, &e.ContextSrc, &e.DurationMS, &filtersApplied, &verification, &e.ErrorMsg,
		); err != nil {
			return nil, err
		}
		e.ParamsSafe = json.RawMessage(paramsSafe)
		if filtersApplied != nil {
			e.FiltersApplied = json.RawMessage(filtersApplied)
		}
		if verification != nil {
			e.Verification = json.RawMessage(verification)
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
			&pa.CallbackURL, &pa.ExpiresAt, &pa.CreatedAt,
		); err != nil {
			return nil, err
		}
		pa.RequestBlob = json.RawMessage(requestBlob)
		pas = append(pas, pa)
	}
	return pas, rows.Err()
}

// ── OAuth ────────────────────────────────────────────────────────────────────

func (s *Store) CreateOAuthClient(ctx context.Context, client *store.OAuthClient) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_clients (id, client_name, redirect_uris)
		VALUES ($1, $2, $3)
	`, client.ID, client.ClientName, mustJSON(client.RedirectURIs))
	return err
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (*store.OAuthClient, error) {
	c := &store.OAuthClient{}
	var uris string
	err := s.pool.QueryRow(ctx,
		`SELECT id, client_name, redirect_uris, created_at FROM oauth_clients WHERE id = $1`,
		clientID,
	).Scan(&c.ID, &c.ClientName, &uris, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("parsing redirect_uris: %w", err)
	}
	return c, nil
}

func (s *Store) SaveAuthorizationCode(ctx context.Context, code *store.OAuthAuthorizationCode) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_authorization_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, scope, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, code.CodeHash, code.ClientID, code.UserID, code.RedirectURI, code.CodeChallenge, code.Scope, code.ExpiresAt)
	return err
}

func (s *Store) ConsumeAuthorizationCode(ctx context.Context, codeHash string) (*store.OAuthAuthorizationCode, error) {
	c := &store.OAuthAuthorizationCode{}
	err := s.pool.QueryRow(ctx,
		`DELETE FROM oauth_authorization_codes WHERE code_hash = $1
		 RETURNING code_hash, client_id, user_id, redirect_uri, code_challenge, scope, expires_at, created_at`,
		codeHash,
	).Scan(&c.CodeHash, &c.ClientID, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Scope, &c.ExpiresAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return c, err
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// nilIfEmpty returns nil if the slice is empty, otherwise returns the slice.
// Used to store NULL rather than empty JSON for optional JSONB columns.
func nilIfEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func scanAgents(rows pgx.Rows) ([]*store.Agent, error) {
	var agents []*store.Agent
	for rows.Next() {
		a := &store.Agent{}
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.TokenHash, &a.CreatedAt); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}
