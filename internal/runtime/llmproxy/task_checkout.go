package llmproxy

import (
	"context"
	"sync"
	"time"
)

// TaskCheckoutKey scopes the agent's current task focus. This is deliberately
// an authorization hint only: decision logic must still verify the checked-out
// task is a valid candidate for the concrete tool/API call.
type TaskCheckoutKey struct {
	UserID  string
	AgentID string
}

// TaskCheckout records the task an agent is currently focused on.
type TaskCheckout struct {
	TaskID    string
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// TaskCheckoutStore persists per-agent task focus for lite-proxy sessions.
type TaskCheckoutStore interface {
	Set(ctx context.Context, key TaskCheckoutKey, taskID string, ttl time.Duration) error
	Get(ctx context.Context, key TaskCheckoutKey) (TaskCheckout, bool, error)
	Clear(ctx context.Context, key TaskCheckoutKey) error
}

type MemoryTaskCheckoutStore struct {
	defaultTTL time.Duration

	mu      sync.Mutex
	entries map[TaskCheckoutKey]TaskCheckout
}

func NewMemoryTaskCheckoutStore(defaultTTL time.Duration) *MemoryTaskCheckoutStore {
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	return &MemoryTaskCheckoutStore{
		defaultTTL: defaultTTL,
		entries:    map[TaskCheckoutKey]TaskCheckout{},
	}
}

func (s *MemoryTaskCheckoutStore) Set(_ context.Context, key TaskCheckoutKey, taskID string, ttl time.Duration) error {
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
	s.entries[key] = TaskCheckout{
		TaskID:    taskID,
		UpdatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	return nil
}

func (s *MemoryTaskCheckoutStore) Get(_ context.Context, key TaskCheckoutKey) (TaskCheckout, bool, error) {
	if key.UserID == "" || key.AgentID == "" {
		return TaskCheckout{}, false, nil
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return TaskCheckout{}, false, nil
	}
	if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
		delete(s.entries, key)
		return TaskCheckout{}, false, nil
	}
	return entry, true, nil
}

func (s *MemoryTaskCheckoutStore) Clear(_ context.Context, key TaskCheckoutKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}

func (s *MemoryTaskCheckoutStore) gcLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(s.entries, key)
		}
	}
}

var _ TaskCheckoutStore = (*MemoryTaskCheckoutStore)(nil)
