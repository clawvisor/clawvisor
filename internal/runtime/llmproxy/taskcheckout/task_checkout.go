package taskcheckout

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrConversationIDRequired is returned by Set when the key has an
// empty ConversationID. The store refuses to write a checkout under a
// user+agent shape that isn't pinned to a specific conversation,
// because such a checkout would be visible to every concurrent
// conversation belonging to the same agent — exactly the cross-task
// leak this package's per-conversation key shape exists to prevent.
var ErrConversationIDRequired = errors.New("task checkout requires a conversation id")

// Key scopes the agent's current task focus. This is deliberately an
// authorization hint only: decision logic must still verify the
// checked-out task is a valid candidate for the concrete tool/API call.
//
// ConversationID partitions focus across conversations sharing a
// Clawvisor token (Conductor workspaces, sub-agents, multiple Claude
// Code sessions in the same installation). It is REQUIRED: a Set with
// an empty ConversationID returns ErrConversationIDRequired, and a Get
// with an empty ConversationID returns not-found. The earlier
// pre-conversation-scoping fallback (writing/reading a shared
// (user, agent) bucket) was removed because it let approvals from one
// conversation silently authorize tool calls from another.
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
	if key.ConversationID == "" {
		return ErrConversationIDRequired
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
	if key.UserID == "" || key.AgentID == "" || key.ConversationID == "" {
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
