package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/base32"
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
	Resolve(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Drop(ctx context.Context, req ResolveRequest) error
}

type MemoryPendingApprovalCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	pending map[pendingApprovalKey]PendingLiteApproval
	now     func() time.Time
}

type pendingApprovalKey struct {
	userID   string
	agentID  string
	provider conversation.Provider
}

func NewMemoryPendingApprovalCache(ttl time.Duration) *MemoryPendingApprovalCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &MemoryPendingApprovalCache{
		ttl:     ttl,
		pending: map[pendingApprovalKey]PendingLiteApproval{},
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
		c.pending = map[pendingApprovalKey]PendingLiteApproval{}
	}
	now := c.now().UTC()
	if pending.ID == "" {
		pending.ID = newLiteApprovalID()
	}
	if pending.CreatedAt.IsZero() {
		pending.CreatedAt = now
	}
	if pending.ExpiresAt.IsZero() {
		pending.ExpiresAt = now.Add(c.ttl)
	}
	key := pending.key()
	var evicted *PendingLiteApproval
	if existing, ok := c.pending[key]; ok {
		existingCopy := existing
		evicted = &existingCopy
	}
	c.pending[key] = pending
	return HoldResult{Pending: pending, Evicted: evicted}, nil
}

func (c *MemoryPendingApprovalCache) Resolve(_ context.Context, req ResolveRequest) (*PendingLiteApproval, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := pendingApprovalKey{userID: req.UserID, agentID: req.AgentID, provider: req.Provider}
	pending, ok := c.pending[key]
	if !ok {
		return nil, nil
	}
	if !pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(c.now().UTC()) {
		delete(c.pending, key)
		return nil, nil
	}
	if req.ApprovalID != "" && pending.ID != req.ApprovalID {
		return nil, nil
	}
	delete(c.pending, key)
	return &pending, nil
}

func (c *MemoryPendingApprovalCache) Drop(_ context.Context, req ResolveRequest) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, pendingApprovalKey{userID: req.UserID, agentID: req.AgentID, provider: req.Provider})
	return nil
}

func (p PendingLiteApproval) key() pendingApprovalKey {
	return pendingApprovalKey{userID: p.UserID, agentID: p.AgentID, provider: p.Provider}
}

func newLiteApprovalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "cv-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano))))[:26]
	}
	return "cv-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}
