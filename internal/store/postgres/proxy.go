package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// -- Proxy instances ----------------------------------------------------------

func (s *Store) CreateProxyInstance(ctx context.Context, p *store.ProxyInstance) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO proxy_instances (id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version)
		VALUES ($1, $2, $3, $4, $5)
	`, p.ID, p.BridgeID, p.TokenHash, p.CACertFingerprint, p.ProxyVersion)
	return err
}

func (s *Store) GetProxyInstanceByHash(ctx context.Context, tokenHash string) (*store.ProxyInstance, error) {
	return s.scanProxyInstance(ctx,
		`SELECT id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version, last_seen_at, created_at, revoked_at
		 FROM proxy_instances WHERE token_hash = $1`, tokenHash)
}

func (s *Store) GetProxyInstanceByID(ctx context.Context, id string) (*store.ProxyInstance, error) {
	return s.scanProxyInstance(ctx,
		`SELECT id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version, last_seen_at, created_at, revoked_at
		 FROM proxy_instances WHERE id = $1`, id)
}

func (s *Store) GetProxyInstanceForBridge(ctx context.Context, bridgeID string) (*store.ProxyInstance, error) {
	return s.scanProxyInstance(ctx,
		`SELECT id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version, last_seen_at, created_at, revoked_at
		 FROM proxy_instances WHERE bridge_id = $1 AND revoked_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`, bridgeID)
}

func (s *Store) scanProxyInstance(ctx context.Context, query, arg string) (*store.ProxyInstance, error) {
	p := &store.ProxyInstance{}
	var lastSeenAt, revokedAt *time.Time
	err := s.pool.QueryRow(ctx, query, arg).Scan(
		&p.ID, &p.BridgeID, &p.TokenHash, &p.CACertFingerprint, &p.ProxyVersion,
		&lastSeenAt, &p.CreatedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.LastSeenAt = lastSeenAt
	p.RevokedAt = revokedAt
	return p, nil
}

func (s *Store) TouchProxyInstanceLastSeen(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE proxy_instances SET last_seen_at = NOW() WHERE id = $1`, id)
	return err
}

func (s *Store) RevokeProxyInstance(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE proxy_instances SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

func (s *Store) SetBridgeProxyEnabled(ctx context.Context, bridgeID, userID string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE bridge_tokens SET proxy_enabled = $1 WHERE id = $2 AND user_id = $3 AND revoked_at IS NULL`,
		enabled, bridgeID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// -- Proxy signing keys -------------------------------------------------------

func (s *Store) RegisterProxySigningKey(ctx context.Context, k *store.ProxySigningKey) error {
	if k.ID == "" {
		k.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO proxy_signing_keys (id, proxy_instance_id, key_id, alg, public_key)
		VALUES ($1, $2, $3, $4, $5)
	`, k.ID, k.ProxyInstanceID, k.KeyID, k.Alg, k.PublicKey)
	return err
}

func (s *Store) GetProxySigningKey(ctx context.Context, proxyInstanceID, keyID string) (*store.ProxySigningKey, error) {
	k := &store.ProxySigningKey{}
	var retiredAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, proxy_instance_id, key_id, alg, public_key, registered_at, retired_at
		FROM proxy_signing_keys
		WHERE proxy_instance_id = $1 AND key_id = $2
	`, proxyInstanceID, keyID).Scan(
		&k.ID, &k.ProxyInstanceID, &k.KeyID, &k.Alg, &k.PublicKey, &k.RegisteredAt, &retiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.RetiredAt = retiredAt
	return k, nil
}

func (s *Store) ListProxySigningKeys(ctx context.Context, proxyInstanceID string) ([]*store.ProxySigningKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, proxy_instance_id, key_id, alg, public_key, registered_at, retired_at
		FROM proxy_signing_keys
		WHERE proxy_instance_id = $1
		ORDER BY registered_at DESC
	`, proxyInstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.ProxySigningKey
	for rows.Next() {
		k := &store.ProxySigningKey{}
		var retiredAt *time.Time
		if err := rows.Scan(&k.ID, &k.ProxyInstanceID, &k.KeyID, &k.Alg, &k.PublicKey, &k.RegisteredAt, &retiredAt); err != nil {
			return nil, err
		}
		k.RetiredAt = retiredAt
		out = append(out, k)
	}
	return out, rows.Err()
}

// -- Transcript events --------------------------------------------------------

// nullString converts "" to NULL for JSONB columns, so Postgres doesn't choke
// on empty strings where JSON is expected.
func nullJSONB(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) InsertTranscriptEvent(ctx context.Context, e *store.TranscriptEvent) error {
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO transcript_events (
			event_id, bridge_id, source, source_version, stream,
			agent_token_id, agent_attribution, conversation_id, provider, direction, role,
			text, tool_calls, tool_results, raw_ref, signature, sig_status, ts
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	`,
		e.EventID, e.BridgeID, e.Source, e.SourceVersion, e.Stream,
		e.AgentTokenID, e.AgentAttribution, e.ConversationID, e.Provider, e.Direction, e.Role,
		e.Text, nullJSONB(e.ToolCallsJSON), nullJSONB(e.ToolResultsJSON),
		nullJSONB(e.RawRefJSON), nullJSONB(e.SignatureJSON), e.SigStatus, ts)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return store.ErrConflict
		}
		return err
	}
	return nil
}

func (s *Store) GetTranscriptEventByID(ctx context.Context, eventID string) (*store.TranscriptEvent, error) {
	e := &store.TranscriptEvent{}
	var toolCalls, toolResults, rawRef, signature *string
	err := s.pool.QueryRow(ctx, `
		SELECT event_id, bridge_id, source, source_version, stream,
		       agent_token_id, agent_attribution, conversation_id, provider, direction, role,
		       text, tool_calls::text, tool_results::text, raw_ref::text, signature::text,
		       sig_status, ts, ingested_at
		FROM transcript_events WHERE event_id = $1
	`, eventID).Scan(
		&e.EventID, &e.BridgeID, &e.Source, &e.SourceVersion, &e.Stream,
		&e.AgentTokenID, &e.AgentAttribution, &e.ConversationID, &e.Provider, &e.Direction, &e.Role,
		&e.Text, &toolCalls, &toolResults, &rawRef, &signature, &e.SigStatus,
		&e.TS, &e.IngestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if toolCalls != nil {
		e.ToolCallsJSON = *toolCalls
	}
	if toolResults != nil {
		e.ToolResultsJSON = *toolResults
	}
	if rawRef != nil {
		e.RawRefJSON = *rawRef
	}
	if signature != nil {
		e.SignatureJSON = *signature
	}
	return e, nil
}

func (s *Store) ListTranscriptEvents(ctx context.Context, f store.TranscriptEventFilter) ([]*store.TranscriptEvent, error) {
	var conditions []string
	var args []any
	idx := 1

	add := func(cond string, arg any) {
		conditions = append(conditions, fmt.Sprintf(cond, idx))
		args = append(args, arg)
		idx++
	}

	if f.BridgeID != "" {
		add("bridge_id = $%d", f.BridgeID)
	}
	if f.ConversationID != "" {
		add("conversation_id = $%d", f.ConversationID)
	}
	if f.Source != "" {
		add("source = $%d", f.Source)
	}
	if f.Stream != "" {
		add("stream = $%d", f.Stream)
	}
	if !f.Since.IsZero() {
		add("ts >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("ts <= $%d", f.Until)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	order := "ASC"
	if f.OrderDescending {
		order = "DESC"
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	query := fmt.Sprintf(`
		SELECT event_id, bridge_id, source, source_version, stream,
		       agent_token_id, agent_attribution, conversation_id, provider, direction, role,
		       text, tool_calls::text, tool_results::text, raw_ref::text, signature::text,
		       sig_status, ts, ingested_at
		FROM transcript_events
		%s
		ORDER BY ts %s
		LIMIT %d
	`, where, order, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*store.TranscriptEvent
	for rows.Next() {
		e := &store.TranscriptEvent{}
		var toolCalls, toolResults, rawRef, signature *string
		if err := rows.Scan(
			&e.EventID, &e.BridgeID, &e.Source, &e.SourceVersion, &e.Stream,
			&e.AgentTokenID, &e.AgentAttribution, &e.ConversationID, &e.Provider, &e.Direction, &e.Role,
			&e.Text, &toolCalls, &toolResults, &rawRef, &signature, &e.SigStatus,
			&e.TS, &e.IngestedAt); err != nil {
			return nil, err
		}
		if toolCalls != nil {
			e.ToolCallsJSON = *toolCalls
		}
		if toolResults != nil {
			e.ToolResultsJSON = *toolResults
		}
		if rawRef != nil {
			e.RawRefJSON = *rawRef
		}
		if signature != nil {
			e.SignatureJSON = *signature
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteExpiredTranscriptEvents(ctx context.Context, olderThan time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM transcript_events WHERE ingested_at < $1`, olderThan)
	return err
}

// -- Transcript anomalies -----------------------------------------------------

func (s *Store) CreateTranscriptAnomaly(ctx context.Context, a *store.TranscriptAnomaly) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO transcript_anomalies (id, bridge_id, conversation_id, kind, detail)
		VALUES ($1, $2, $3, $4, $5)
	`, a.ID, a.BridgeID, a.ConversationID, a.Kind, nullJSONB(a.DetailJSON))
	return err
}

func (s *Store) ListTranscriptAnomalies(ctx context.Context, bridgeID string, limit int) ([]*store.TranscriptAnomaly, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, bridge_id, conversation_id, kind, detail::text, detected_at, resolved_at, resolved_by
		FROM transcript_anomalies
		WHERE bridge_id = $1
		ORDER BY detected_at DESC
		LIMIT $2
	`, bridgeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.TranscriptAnomaly
	for rows.Next() {
		a := &store.TranscriptAnomaly{}
		var detail, resolvedBy *string
		var resolvedAt *time.Time
		if err := rows.Scan(&a.ID, &a.BridgeID, &a.ConversationID, &a.Kind, &detail,
			&a.DetectedAt, &resolvedAt, &resolvedBy); err != nil {
			return nil, err
		}
		if detail != nil {
			a.DetailJSON = *detail
		}
		if resolvedBy != nil {
			a.ResolvedBy = *resolvedBy
		}
		a.ResolvedAt = resolvedAt
		out = append(out, a)
	}
	return out, rows.Err()
}
