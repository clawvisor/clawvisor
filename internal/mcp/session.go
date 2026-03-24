package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// SessionStore persists MCP sessions in the database so they survive restarts.
type SessionStore struct {
	store  store.Store
	ttl    time.Duration
	logger *slog.Logger
}

// NewSessionStore creates a database-backed session store with the given TTL.
func NewSessionStore(st store.Store, ttl time.Duration, logger *slog.Logger) *SessionStore {
	return &SessionStore{
		store:  st,
		ttl:    ttl,
		logger: logger,
	}
}

// Create generates a new session, persists it, and returns its ID.
func (s *SessionStore) Create(ctx context.Context) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)

	expiresAt := time.Now().Add(s.ttl)
	if err := s.store.CreateMCPSession(ctx, id, expiresAt); err != nil {
		return "", err
	}
	return id, nil
}

// Valid checks whether the session ID exists and is not expired.
func (s *SessionStore) Valid(ctx context.Context, id string) bool {
	ok, err := s.store.MCPSessionValid(ctx, id)
	if err != nil {
		s.logger.Warn("mcp session lookup failed", "session_id", id, "err", err)
		return false
	}
	return ok
}

// RunCleanup periodically removes expired sessions. Blocks until done is closed.
func (s *SessionStore) RunCleanup(done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := s.store.CleanupMCPSessions(context.Background()); err != nil {
				s.logger.Warn("mcp session cleanup failed", "err", err)
			}
		}
	}
}
