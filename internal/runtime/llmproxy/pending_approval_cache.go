package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

type PendingLiteApproval struct {
	ID       string
	UserID   string
	AgentID  string
	Provider conversation.Provider
	ToolUse  conversation.ToolUse

	Inspector   inspector.Verdict
	Fingerprint runtimedecision.DecisionFingerprint

	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type ResolveRequest struct {
	UserID     string
	AgentID    string
	Provider   conversation.Provider
	ApprovalID string
}

type HoldResult struct {
	Pending PendingLiteApproval
	Evicted *PendingLiteApproval
}

type PendingApprovalCache interface {
	Hold(ctx context.Context, pending PendingLiteApproval) (HoldResult, error)
	Peek(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Resolve(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Drop(ctx context.Context, req ResolveRequest) error
}

type MemoryPendingApprovalCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	pending map[pendingApprovalKey][]PendingLiteApproval
	now     func() time.Time
}

type pendingApprovalKey struct {
	userID   string
	agentID  string
	provider conversation.Provider
}

var liteApprovalRandRead = rand.Read

func NewMemoryPendingApprovalCache(ttl time.Duration) *MemoryPendingApprovalCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &MemoryPendingApprovalCache{
		ttl:     ttl,
		max:     10,
		pending: map[pendingApprovalKey][]PendingLiteApproval{},
		now:     time.Now,
	}
}

func (c *MemoryPendingApprovalCache) Hold(_ context.Context, pending PendingLiteApproval) (HoldResult, error) {
	if c == nil {
		return HoldResult{Pending: pending}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[pendingApprovalKey][]PendingLiteApproval{}
	}
	now := c.now().UTC()
	if pending.ID == "" {
		id, err := newLiteApprovalID()
		if err != nil {
			return HoldResult{}, err
		}
		pending.ID = id
	}
	if pending.CreatedAt.IsZero() {
		pending.CreatedAt = now
	}
	if pending.ExpiresAt.IsZero() {
		pending.ExpiresAt = now.Add(c.ttl)
	}
	key := pending.key()
	var evicted *PendingLiteApproval
	items := c.pruneExpiredLocked(key, now)
	if c.max <= 0 {
		c.max = 10
	}
	for len(items) >= c.max {
		existingCopy := items[0]
		evicted = &existingCopy
		items = items[1:]
	}
	items = append(items, pending)
	c.pending[key] = items
	return HoldResult{Pending: pending, Evicted: evicted}, nil
}

func (c *MemoryPendingApprovalCache) Resolve(_ context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, index, items := c.findLocked(req)
	if pending == nil {
		return nil, nil
	}
	key := pendingApprovalKey{userID: req.UserID, agentID: req.AgentID, provider: req.Provider}
	items = append(items[:index], items[index+1:]...)
	if len(items) == 0 {
		delete(c.pending, key)
	} else {
		c.pending[key] = items
	}
	return pending, nil
}

func (c *MemoryPendingApprovalCache) Peek(_ context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, _, _ := c.findLocked(req)
	return pending, nil
}

func (c *MemoryPendingApprovalCache) findLocked(req ResolveRequest) (*PendingLiteApproval, int, []PendingLiteApproval) {
	key := pendingApprovalKey{userID: req.UserID, agentID: req.AgentID, provider: req.Provider}
	items := c.pruneExpiredLocked(key, c.now().UTC())
	if len(items) == 0 {
		return nil, -1, items
	}
	index := 0
	if req.ApprovalID != "" {
		index = -1
		for i, pending := range items {
			if pending.ID == req.ApprovalID {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, -1, items
		}
	}
	pending := items[index]
	return &pending, index, items
}

func (c *MemoryPendingApprovalCache) Drop(_ context.Context, req ResolveRequest) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := pendingApprovalKey{userID: req.UserID, agentID: req.AgentID, provider: req.Provider}
	if req.ApprovalID == "" {
		delete(c.pending, key)
		return nil
	}
	items := c.pending[key]
	for i, pending := range items {
		if pending.ID == req.ApprovalID {
			items = append(items[:i], items[i+1:]...)
			if len(items) == 0 {
				delete(c.pending, key)
			} else {
				c.pending[key] = items
			}
			return nil
		}
	}
	return nil
}

func (p PendingLiteApproval) key() pendingApprovalKey {
	return pendingApprovalKey{userID: p.UserID, agentID: p.AgentID, provider: p.Provider}
}

func (c *MemoryPendingApprovalCache) pruneExpiredLocked(key pendingApprovalKey, now time.Time) []PendingLiteApproval {
	items := c.pending[key]
	if len(items) == 0 {
		return nil
	}
	kept := items[:0]
	for _, pending := range items {
		if pending.ExpiresAt.IsZero() || pending.ExpiresAt.After(now) {
			kept = append(kept, pending)
		}
	}
	if len(kept) == 0 {
		delete(c.pending, key)
		return nil
	}
	c.pending[key] = kept
	return kept
}

func newLiteApprovalID() (string, error) {
	var b [16]byte
	if _, err := liteApprovalRandRead(b[:]); err != nil {
		return "", fmt.Errorf("generate approval id: %w", err)
	}
	return "cv-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}
