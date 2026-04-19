package handlers

import (
	"context"
	"sync"
	"time"
)

// ExtractionTracker tracks in-flight chain context extractions so that a
// request arriving on server B can tell whether server A is still writing
// facts for the same session. Without this, the 300ms-gap follow-up call
// races the extraction goroutine and spuriously rejects on chain-context.
//
// Keyed by (taskID, sessionID). Multiple concurrent extractions per session
// are tracked by auditID so a late MarkDone doesn't clear an unrelated one.
type ExtractionTracker interface {
	MarkPending(ctx context.Context, taskID, sessionID, auditID string)
	MarkDone(ctx context.Context, taskID, sessionID, auditID string)
	HasPending(ctx context.Context, taskID, sessionID string) bool
}

// memoryExtractionTracker is the default single-instance implementation.
type memoryExtractionTracker struct {
	mu       sync.Mutex
	pending  map[string]map[string]time.Time // sessionKey → auditID → deadline
	entryTTL time.Duration
}

// NewMemoryExtractionTracker creates an in-memory tracker. Entries older
// than ttl are treated as expired (handles orphaned entries from crashed
// goroutines).
func NewMemoryExtractionTracker(ttl time.Duration) ExtractionTracker {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &memoryExtractionTracker{
		pending:  make(map[string]map[string]time.Time),
		entryTTL: ttl,
	}
}

func (t *memoryExtractionTracker) MarkPending(_ context.Context, taskID, sessionID, auditID string) {
	if taskID == "" || sessionID == "" || auditID == "" {
		return
	}
	key := taskID + "|" + sessionID
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.pending[key]
	if m == nil {
		m = make(map[string]time.Time)
		t.pending[key] = m
	}
	m[auditID] = time.Now().Add(t.entryTTL)
}

func (t *memoryExtractionTracker) MarkDone(_ context.Context, taskID, sessionID, auditID string) {
	if taskID == "" || sessionID == "" || auditID == "" {
		return
	}
	key := taskID + "|" + sessionID
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := t.pending[key]; m != nil {
		delete(m, auditID)
		if len(m) == 0 {
			delete(t.pending, key)
		}
	}
}

func (t *memoryExtractionTracker) HasPending(_ context.Context, taskID, sessionID string) bool {
	if taskID == "" || sessionID == "" {
		return false
	}
	key := taskID + "|" + sessionID
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.pending[key]
	if len(m) == 0 {
		return false
	}
	now := time.Now()
	for auditID, deadline := range m {
		if deadline.After(now) {
			return true
		}
		delete(m, auditID)
	}
	if len(m) == 0 {
		delete(t.pending, key)
	}
	return false
}
