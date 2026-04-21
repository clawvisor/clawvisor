package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// -- Proxy instances ----------------------------------------------------------

func (s *Store) CreateProxyInstance(ctx context.Context, p *store.ProxyInstance) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO proxy_instances (id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version)
		VALUES (?, ?, ?, ?, ?)
	`, p.ID, p.BridgeID, p.TokenHash, p.CACertFingerprint, p.ProxyVersion)
	return err
}

func (s *Store) GetProxyInstanceByHash(ctx context.Context, tokenHash string) (*store.ProxyInstance, error) {
	return s.scanProxyInstance(ctx,
		`SELECT id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version, last_seen_at, created_at, revoked_at
		 FROM proxy_instances WHERE token_hash = ?`, tokenHash)
}

func (s *Store) GetProxyInstanceByID(ctx context.Context, id string) (*store.ProxyInstance, error) {
	return s.scanProxyInstance(ctx,
		`SELECT id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version, last_seen_at, created_at, revoked_at
		 FROM proxy_instances WHERE id = ?`, id)
}

func (s *Store) GetProxyInstanceForBridge(ctx context.Context, bridgeID string) (*store.ProxyInstance, error) {
	return s.scanProxyInstance(ctx,
		`SELECT id, bridge_id, token_hash, ca_cert_fingerprint, proxy_version, last_seen_at, created_at, revoked_at
		 FROM proxy_instances WHERE bridge_id = ? AND revoked_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`, bridgeID)
}

func (s *Store) scanProxyInstance(ctx context.Context, query, arg string) (*store.ProxyInstance, error) {
	p := &store.ProxyInstance{}
	var createdAt string
	var lastSeenAt, revokedAt sql.NullString
	err := s.db.QueryRowContext(ctx, query, arg).Scan(
		&p.ID, &p.BridgeID, &p.TokenHash, &p.CACertFingerprint, &p.ProxyVersion,
		&lastSeenAt, &createdAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(createdAt)
	if lastSeenAt.Valid {
		t := parseTime(lastSeenAt.String)
		p.LastSeenAt = &t
	}
	if revokedAt.Valid {
		t := parseTime(revokedAt.String)
		p.RevokedAt = &t
	}
	return p, nil
}

func (s *Store) TouchProxyInstanceLastSeen(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE proxy_instances SET last_seen_at = datetime('now') WHERE id = ?`, id)
	return err
}

func (s *Store) RevokeProxyInstance(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE proxy_instances SET revoked_at = datetime('now') WHERE id = ? AND revoked_at IS NULL`, id)
	return err
}

func (s *Store) SetBridgeProxyEnabled(ctx context.Context, bridgeID, userID string, enabled bool) error {
	flag := 0
	if enabled {
		flag = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE bridge_tokens SET proxy_enabled = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		flag, bridgeID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// -- Proxy signing keys -------------------------------------------------------

func (s *Store) RegisterProxySigningKey(ctx context.Context, k *store.ProxySigningKey) error {
	if k.ID == "" {
		k.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO proxy_signing_keys (id, proxy_instance_id, key_id, alg, public_key)
		VALUES (?, ?, ?, ?, ?)
	`, k.ID, k.ProxyInstanceID, k.KeyID, k.Alg, k.PublicKey)
	return err
}

func (s *Store) GetProxySigningKey(ctx context.Context, proxyInstanceID, keyID string) (*store.ProxySigningKey, error) {
	k := &store.ProxySigningKey{}
	var registeredAt string
	var retiredAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, proxy_instance_id, key_id, alg, public_key, registered_at, retired_at
		FROM proxy_signing_keys
		WHERE proxy_instance_id = ? AND key_id = ?
	`, proxyInstanceID, keyID).Scan(
		&k.ID, &k.ProxyInstanceID, &k.KeyID, &k.Alg, &k.PublicKey, &registeredAt, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.RegisteredAt = parseTime(registeredAt)
	if retiredAt.Valid {
		t := parseTime(retiredAt.String)
		k.RetiredAt = &t
	}
	return k, nil
}

func (s *Store) ListProxySigningKeys(ctx context.Context, proxyInstanceID string) ([]*store.ProxySigningKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, proxy_instance_id, key_id, alg, public_key, registered_at, retired_at
		FROM proxy_signing_keys
		WHERE proxy_instance_id = ?
		ORDER BY registered_at DESC
	`, proxyInstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.ProxySigningKey
	for rows.Next() {
		k := &store.ProxySigningKey{}
		var registeredAt string
		var retiredAt sql.NullString
		if err := rows.Scan(&k.ID, &k.ProxyInstanceID, &k.KeyID, &k.Alg, &k.PublicKey, &registeredAt, &retiredAt); err != nil {
			return nil, err
		}
		k.RegisteredAt = parseTime(registeredAt)
		if retiredAt.Valid {
			t := parseTime(retiredAt.String)
			k.RetiredAt = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// -- Transcript events --------------------------------------------------------

func (s *Store) InsertTranscriptEvent(ctx context.Context, e *store.TranscriptEvent) error {
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transcript_events (
			event_id, bridge_id, source, source_version, stream,
			agent_token_id, agent_attribution, conversation_id, provider, direction, role,
			text, tool_calls, tool_results, raw_ref, signature, sig_status, ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		e.EventID, e.BridgeID, e.Source, e.SourceVersion, e.Stream,
		e.AgentTokenID, e.AgentAttribution, e.ConversationID, e.Provider, e.Direction, e.Role,
		e.Text, e.ToolCallsJSON, e.ToolResultsJSON, e.RawRefJSON, e.SignatureJSON, e.SigStatus,
		ts.UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return store.ErrConflict
		}
		return err
	}
	return nil
}

func (s *Store) GetTranscriptEventByID(ctx context.Context, eventID string) (*store.TranscriptEvent, error) {
	e := &store.TranscriptEvent{}
	var ts, ingestedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT event_id, bridge_id, source, source_version, stream,
		       agent_token_id, agent_attribution, conversation_id, provider, direction, role,
		       text, tool_calls, tool_results, raw_ref, signature, sig_status, ts, ingested_at
		FROM transcript_events WHERE event_id = ?
	`, eventID).Scan(
		&e.EventID, &e.BridgeID, &e.Source, &e.SourceVersion, &e.Stream,
		&e.AgentTokenID, &e.AgentAttribution, &e.ConversationID, &e.Provider, &e.Direction, &e.Role,
		&e.Text, &e.ToolCallsJSON, &e.ToolResultsJSON, &e.RawRefJSON, &e.SignatureJSON, &e.SigStatus,
		&ts, &ingestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.TS = parseTime(ts)
	e.IngestedAt = parseTime(ingestedAt)
	return e, nil
}

func (s *Store) ListTranscriptEvents(ctx context.Context, f store.TranscriptEventFilter) ([]*store.TranscriptEvent, error) {
	var conditions []string
	var args []any

	if f.BridgeID != "" {
		conditions = append(conditions, fmt.Sprintf("bridge_id = ?"))
		args = append(args, f.BridgeID)
	}
	if f.ConversationID != "" {
		conditions = append(conditions, fmt.Sprintf("conversation_id = ?"))
		args = append(args, f.ConversationID)
	}
	if f.Source != "" {
		conditions = append(conditions, fmt.Sprintf("source = ?"))
		args = append(args, f.Source)
	}
	if f.Stream != "" {
		conditions = append(conditions, fmt.Sprintf("stream = ?"))
		args = append(args, f.Stream)
	}
	if !f.Since.IsZero() {
		conditions = append(conditions, fmt.Sprintf("ts >= ?"))
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		conditions = append(conditions, fmt.Sprintf("ts <= ?"))
		args = append(args, f.Until.UTC().Format(time.RFC3339Nano))
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
		       text, tool_calls, tool_results, raw_ref, signature, sig_status, ts, ingested_at
		FROM transcript_events
		%s
		ORDER BY ts %s
		LIMIT %d
	`, where, order, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*store.TranscriptEvent
	for rows.Next() {
		e := &store.TranscriptEvent{}
		var ts, ingestedAt string
		if err := rows.Scan(
			&e.EventID, &e.BridgeID, &e.Source, &e.SourceVersion, &e.Stream,
			&e.AgentTokenID, &e.AgentAttribution, &e.ConversationID, &e.Provider, &e.Direction, &e.Role,
			&e.Text, &e.ToolCallsJSON, &e.ToolResultsJSON, &e.RawRefJSON, &e.SignatureJSON, &e.SigStatus,
			&ts, &ingestedAt); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		e.IngestedAt = parseTime(ingestedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteExpiredTranscriptEvents(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM transcript_events WHERE ingested_at < ?`,
		olderThan.UTC().Format(time.RFC3339Nano))
	return err
}

// -- Transcript anomalies -----------------------------------------------------

func (s *Store) CreateTranscriptAnomaly(ctx context.Context, a *store.TranscriptAnomaly) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transcript_anomalies (id, bridge_id, conversation_id, kind, detail)
		VALUES (?, ?, ?, ?, ?)
	`, a.ID, a.BridgeID, a.ConversationID, a.Kind, a.DetailJSON)
	return err
}

func (s *Store) ListTranscriptAnomalies(ctx context.Context, bridgeID string, limit int) ([]*store.TranscriptAnomaly, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, bridge_id, conversation_id, kind, detail, detected_at, resolved_at, resolved_by
		FROM transcript_anomalies
		WHERE bridge_id = ?
		ORDER BY detected_at DESC
		LIMIT ?
	`, bridgeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.TranscriptAnomaly
	for rows.Next() {
		a := &store.TranscriptAnomaly{}
		var detectedAt string
		var resolvedAt, resolvedBy sql.NullString
		if err := rows.Scan(&a.ID, &a.BridgeID, &a.ConversationID, &a.Kind, &a.DetailJSON,
			&detectedAt, &resolvedAt, &resolvedBy); err != nil {
			return nil, err
		}
		a.DetectedAt = parseTime(detectedAt)
		if resolvedAt.Valid {
			t := parseTime(resolvedAt.String)
			a.ResolvedAt = &t
		}
		if resolvedBy.Valid {
			a.ResolvedBy = resolvedBy.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
