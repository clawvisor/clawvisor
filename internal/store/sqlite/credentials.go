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

// -- InjectableCredentials -----------------------------------------------

func (s *Store) UpsertInjectableCredential(ctx context.Context, c *store.InjectableCredential) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	agents, _ := json.Marshal(c.UsableByAgents)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO injectable_credentials (id, user_id, credential_ref, vault_key, usable_by_agents)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, credential_ref) DO UPDATE SET
			vault_key = excluded.vault_key,
			usable_by_agents = excluded.usable_by_agents,
			rotated_at = datetime('now'),
			revoked_at = NULL
	`, c.ID, c.UserID, c.CredentialRef, c.VaultKey, string(agents))
	return err
}

func (s *Store) GetInjectableCredential(ctx context.Context, userID, credentialRef string) (*store.InjectableCredential, error) {
	return s.scanInjectableCredential(ctx,
		`SELECT id, user_id, credential_ref, vault_key, usable_by_agents, created_at, rotated_at, revoked_at
		 FROM injectable_credentials WHERE user_id = ? AND credential_ref = ?`,
		userID, credentialRef)
}

func (s *Store) scanInjectableCredential(ctx context.Context, query string, args ...any) (*store.InjectableCredential, error) {
	c := &store.InjectableCredential{}
	var createdAt string
	var rotatedAt, revokedAt sql.NullString
	var agentsJSON string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&c.ID, &c.UserID, &c.CredentialRef, &c.VaultKey, &agentsJSON, &createdAt, &rotatedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = parseTime(createdAt)
	if rotatedAt.Valid {
		t := parseTime(rotatedAt.String)
		c.RotatedAt = &t
	}
	if revokedAt.Valid {
		t := parseTime(revokedAt.String)
		c.RevokedAt = &t
	}
	if agentsJSON != "" {
		_ = json.Unmarshal([]byte(agentsJSON), &c.UsableByAgents)
	}
	return c, nil
}

func (s *Store) ListInjectableCredentials(ctx context.Context, userID string) ([]*store.InjectableCredential, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, credential_ref, vault_key, usable_by_agents, created_at, rotated_at, revoked_at
		FROM injectable_credentials WHERE user_id = ?
		ORDER BY credential_ref
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InjectableCredential
	for rows.Next() {
		c := &store.InjectableCredential{}
		var createdAt string
		var rotatedAt, revokedAt sql.NullString
		var agentsJSON string
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialRef, &c.VaultKey, &agentsJSON, &createdAt, &rotatedAt, &revokedAt); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(createdAt)
		if rotatedAt.Valid {
			t := parseTime(rotatedAt.String)
			c.RotatedAt = &t
		}
		if revokedAt.Valid {
			t := parseTime(revokedAt.String)
			c.RevokedAt = &t
		}
		if agentsJSON != "" {
			_ = json.Unmarshal([]byte(agentsJSON), &c.UsableByAgents)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInjectableCredential(ctx context.Context, userID, credentialRef string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE injectable_credentials SET revoked_at = datetime('now')
		WHERE user_id = ? AND credential_ref = ? AND revoked_at IS NULL
	`, userID, credentialRef)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// -- Injection rules ------------------------------------------------------

func (s *Store) CreateInjectionRule(ctx context.Context, r *store.InjectionRule) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.InjectTemplate == "" {
		r.InjectTemplate = "{{credential}}"
	}
	if r.PathPattern == "" {
		r.PathPattern = "*"
	}
	if r.Method == "" {
		r.Method = "*"
	}
	enabled := 1
	if !r.Enabled {
		enabled = 0
	}
	var userID sql.NullString
	if r.UserID != "" {
		userID = sql.NullString{String: r.UserID, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO injection_rules (id, user_id, host_pattern, path_pattern, method,
			inject_style, inject_target, inject_template, credential_ref, priority, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, userID, r.HostPattern, r.PathPattern, r.Method,
		r.InjectStyle, r.InjectTarget, r.InjectTemplate, r.CredentialRef, r.Priority, enabled)
	return err
}

func (s *Store) ListInjectionRules(ctx context.Context, userID string) ([]*store.InjectionRule, error) {
	// Returns built-in (user_id NULL) + user-level rules, user overrides
	// taking precedence via priority ordering.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, host_pattern, path_pattern, method,
		       inject_style, inject_target, inject_template, credential_ref, priority, enabled, created_at
		FROM injection_rules
		WHERE (user_id = ? OR user_id IS NULL) AND enabled = 1
		ORDER BY priority ASC, created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InjectionRule
	for rows.Next() {
		r := &store.InjectionRule{}
		var uid sql.NullString
		var enabled int
		var createdAt string
		if err := rows.Scan(&r.ID, &uid, &r.HostPattern, &r.PathPattern, &r.Method,
			&r.InjectStyle, &r.InjectTarget, &r.InjectTemplate, &r.CredentialRef,
			&r.Priority, &enabled, &createdAt); err != nil {
			return nil, err
		}
		if uid.Valid {
			r.UserID = uid.String
		}
		r.Enabled = enabled != 0
		r.CreatedAt = parseTime(createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// -- Usage log -----------------------------------------------------------

func (s *Store) LogCredentialUsage(ctx context.Context, u *store.CredentialUsageRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO credential_usage_log (agent_token_id, credential_ref, destination_host, destination_path, decision, request_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, u.AgentTokenID, u.CredentialRef, u.DestinationHost, u.DestinationPath, u.Decision, u.RequestID)
	return err
}

func (s *Store) ListCredentialUsage(ctx context.Context, userID string, since time.Time, limit int) ([]*store.CredentialUsageRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	// userID filter is via credential_ref → injectable_credentials.user_id.
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.id, l.ts, l.agent_token_id, l.credential_ref, l.destination_host,
		       l.destination_path, l.decision, l.request_id
		FROM credential_usage_log l
		JOIN injectable_credentials c ON c.credential_ref = l.credential_ref
		WHERE c.user_id = ? AND l.ts >= ?
		ORDER BY l.ts DESC
		LIMIT ?
	`, userID, since.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.CredentialUsageRecord
	for rows.Next() {
		u := &store.CredentialUsageRecord{}
		var ts string
		if err := rows.Scan(&u.ID, &ts, &u.AgentTokenID, &u.CredentialRef,
			&u.DestinationHost, &u.DestinationPath, &u.Decision, &u.RequestID); err != nil {
			return nil, err
		}
		u.TS = parseTime(ts)
		out = append(out, u)
	}
	return out, rows.Err()
}
