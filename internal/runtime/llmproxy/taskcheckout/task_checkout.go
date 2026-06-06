package taskcheckout

import (
	"context"
	"sync"
	"time"
)

// Key scopes the agent's current task focus. This is deliberately
// an authorization hint only: decision logic must still verify the checked-out
// task is a valid candidate for the concrete tool/API call.
//
// ConversationID partitions focus across conversations sharing a Clawvisor
// token (Conductor workspaces, sub-agents, multiple Claude Code sessions in
// the same installation). Approving a task in conversation B no longer
// overwrites conversation A's focus. Empty ConversationID falls back to the
// pre-conversation-scoping key shape (user+agent only), matching old clients
// that never surfaced a session identifier on the wire.
type Key struct {
	UserID         string
	AgentID        string
	ConversationID string
}

// Checkout records the task an agent is currently focused on.
type Checkout struct {
	TaskID    string
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// Store persists per-agent task focus for lite-proxy sessions.
type Store interface {
	Set(ctx context.Context, key Key, taskID string, ttl time.Duration) error
	Get(ctx context.Context, key Key) (Checkout, bool, error)
	Clear(ctx context.Context, key Key) error
}

type MemoryStore struct {
	defaultTTL time.Duration

	mu      sync.Mutex
	entries map[Key]Checkout
}

func NewMemoryStore(defaultTTL time.Duration) *MemoryStore {
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	return &MemoryStore{
		defaultTTL: defaultTTL,
		entries:    map[Key]Checkout{},
	}
}

func (s *MemoryStore) Set(_ context.Context, key Key, taskID string, ttl time.Duration) error {
	if key.UserID == "" || key.AgentID == "" || taskID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	s.entries[key] = Checkout{
		TaskID:    taskID,
		UpdatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	return nil
}

func (s *MemoryStore) Get(_ context.Context, key Key) (Checkout, bool, error) {
	if key.UserID == "" || key.AgentID == "" {
		return Checkout{}, false, nil
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return Checkout{}, false, nil
	}
	if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
		delete(s.entries, key)
		return Checkout{}, false, nil
	}
	return entry, true, nil
}

func (s *MemoryStore) Clear(_ context.Context, key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}

func (s *MemoryStore) gcLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(s.entries, key)
		}
	}
}

var _ Store = (*MemoryStore)(nil)
