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

func (s *Store) UpsertInjectableCredential(ctx context.Context, c *store.InjectableCredential) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	agents, _ := json.Marshal(c.UsableByAgents)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO injectable_credentials (id, user_id, credential_ref, vault_key, usable_by_agents)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (user_id, credential_ref) DO UPDATE SET
			vault_key = EXCLUDED.vault_key,
			usable_by_agents = EXCLUDED.usable_by_agents,
			rotated_at = NOW(),
			revoked_at = NULL
	`, c.ID, c.UserID, c.CredentialRef, c.VaultKey, string(agents))
	return err
}

func (s *Store) GetInjectableCredential(ctx context.Context, userID, credentialRef string) (*store.InjectableCredential, error) {
	return s.scanInjectableCredential(ctx,
		`SELECT id, user_id, credential_ref, vault_key, usable_by_agents::text, created_at, rotated_at, revoked_at
		 FROM injectable_credentials WHERE user_id = $1 AND credential_ref = $2`,
		userID, credentialRef)
}

func (s *Store) scanInjectableCredential(ctx context.Context, query string, args ...any) (*store.InjectableCredential, error) {
	c := &store.InjectableCredential{}
	var agentsJSON *string
	var rotatedAt, revokedAt *time.Time
	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&c.ID, &c.UserID, &c.CredentialRef, &c.VaultKey, &agentsJSON,
		&c.CreatedAt, &rotatedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.RotatedAt = rotatedAt
	c.RevokedAt = revokedAt
	if agentsJSON != nil && *agentsJSON != "" {
		_ = json.Unmarshal([]byte(*agentsJSON), &c.UsableByAgents)
	}
	return c, nil
}

func (s *Store) ListInjectableCredentials(ctx context.Context, userID string) ([]*store.InjectableCredential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, credential_ref, vault_key, usable_by_agents::text, created_at, rotated_at, revoked_at
		FROM injectable_credentials WHERE user_id = $1 ORDER BY credential_ref
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InjectableCredential
	for rows.Next() {
		c := &store.InjectableCredential{}
		var agentsJSON *string
		var rotatedAt, revokedAt *time.Time
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialRef, &c.VaultKey,
			&agentsJSON, &c.CreatedAt, &rotatedAt, &revokedAt); err != nil {
			return nil, err
		}
		c.RotatedAt = rotatedAt
		c.RevokedAt = revokedAt
		if agentsJSON != nil && *agentsJSON != "" {
			_ = json.Unmarshal([]byte(*agentsJSON), &c.UsableByAgents)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInjectableCredential(ctx context.Context, userID, credentialRef string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE injectable_credentials SET revoked_at = NOW()
		WHERE user_id = $1 AND credential_ref = $2 AND revoked_at IS NULL
	`, userID, credentialRef)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

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
	var userID any
	if r.UserID != "" {
		userID = r.UserID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO injection_rules (id, user_id, host_pattern, path_pattern, method,
			inject_style, inject_target, inject_template, credential_ref, priority, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, r.ID, userID, r.HostPattern, r.PathPattern, r.Method,
		r.InjectStyle, r.InjectTarget, r.InjectTemplate, r.CredentialRef, r.Priority, r.Enabled)
	return err
}

func (s *Store) ListInjectionRules(ctx context.Context, userID string) ([]*store.InjectionRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(user_id, ''), host_pattern, path_pattern, method,
		       inject_style, inject_target, inject_template, credential_ref, priority, enabled, created_at
		FROM injection_rules
		WHERE (user_id = $1 OR user_id IS NULL) AND enabled = TRUE
		ORDER BY priority ASC, created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.InjectionRule
	for rows.Next() {
		r := &store.InjectionRule{}
		if err := rows.Scan(&r.ID, &r.UserID, &r.HostPattern, &r.PathPattern, &r.Method,
			&r.InjectStyle, &r.InjectTarget, &r.InjectTemplate, &r.CredentialRef,
			&r.Priority, &r.Enabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) LogCredentialUsage(ctx context.Context, u *store.CredentialUsageRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO credential_usage_log (agent_token_id, credential_ref, destination_host, destination_path, decision, request_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, u.AgentTokenID, u.CredentialRef, u.DestinationHost, u.DestinationPath, u.Decision, u.RequestID)
	return err
}

func (s *Store) ListCredentialUsage(ctx context.Context, userID string, since time.Time, limit int) ([]*store.CredentialUsageRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.ts, l.agent_token_id, l.credential_ref, l.destination_host,
		       l.destination_path, l.decision, l.request_id
		FROM credential_usage_log l
		JOIN injectable_credentials c ON c.credential_ref = l.credential_ref
		WHERE c.user_id = $1 AND l.ts >= $2
		ORDER BY l.ts DESC
		LIMIT $3
	`, userID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.CredentialUsageRecord
	for rows.Next() {
		u := &store.CredentialUsageRecord{}
		if err := rows.Scan(&u.ID, &u.TS, &u.AgentTokenID, &u.CredentialRef,
			&u.DestinationHost, &u.DestinationPath, &u.Decision, &u.RequestID); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
