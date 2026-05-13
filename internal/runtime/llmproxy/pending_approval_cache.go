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
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// PendingApprovalStage is the per-hold state in the inline-task-approval
// two-step flow. Empty / StageTool is the standard one-step approval
// (existing behavior). The other stages run the two-step flow:
//
//	StageTool ──user types "task"──► StageAwaitingTaskDefinition
//	                                 │
//	                                 model emits POST /control/tasks
//	                                 ▼
//	                                 (new hold) StageAwaitingTaskApproval
//	                                 │
//	                                 user types "approve"
//	                                 ▼
//	                                 create task + release both holds
type PendingApprovalStage string

const (
	// StageTool — the original tool_use hold awaiting approve/deny/task.
	StageTool PendingApprovalStage = ""
	// StageAwaitingTaskDefinition — user typed "task". The same hold's
	// ToolUse field still points at the ORIGINAL tool. We're waiting for
	// the model to emit a POST /control/tasks tool_use that defines the
	// task that should cover this work.
	StageAwaitingTaskDefinition PendingApprovalStage = "awaiting_task_definition"
	// StageAwaitingTaskApproval — model has emitted the task definition.
	// The hold's ToolUse is the task-creation POST itself; AwaitingTaskFor
	// links back to the original tool hold. We're waiting for the user
	// to approve/deny.
	StageAwaitingTaskApproval PendingApprovalStage = "awaiting_task_approval"
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

	// Stage controls the two-step inline-task flow. Empty == StageTool
	// preserves legacy behavior so existing callers don't need to set it.
	Stage PendingApprovalStage

	// AwaitingTaskFor is the ID of the original tool-use hold this task
	// definition will cover. Set ONLY on the inner StageAwaitingTaskApproval
	// hold; empty otherwise. The release path uses this to find the
	// upstream bash/tool hold and release-or-deny it in cascade.
	AwaitingTaskFor string

	// TaskDefinition is the parsed body of the POST /control/tasks the
	// model emitted at StageAwaitingTaskDefinition. Used both to render the
	// inline approval prompt and to create the task once the user approves.
	// nil at the other stages.
	TaskDefinition *runtimetasks.TaskCreateRequest
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

// UpdateRequest carries a stage + optional ExpiresAt override applied to a
// specific (user, agent, provider, id) hold. Zero ExpiresAt leaves expiry
// untouched. Used to (a) transition the original tool hold to
// StageAwaitingTaskDefinition when the user types "task" and (b) refresh
// the original hold's TTL when the inner task-approval hold is created so
// the two-step gesture has the full TTL window for the second approve.
type UpdateRequest struct {
	UserID     string
	AgentID    string
	Provider   conversation.Provider
	ApprovalID string
	Stage      PendingApprovalStage
	ExpiresAt  time.Time
}

type PendingApprovalCache interface {
	Hold(ctx context.Context, pending PendingLiteApproval) (HoldResult, error)
	Peek(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Resolve(ctx context.Context, req ResolveRequest) (*PendingLiteApproval, error)
	Drop(ctx context.Context, req ResolveRequest) error
	// Update mutates Stage and optionally ExpiresAt on the matching hold.
	// Returns the updated entry, or nil if no match was found.
	Update(ctx context.Context, req UpdateRequest) (*PendingLiteApproval, error)
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

func (c *MemoryPendingApprovalCache) Update(_ context.Context, req UpdateRequest) (*PendingLiteApproval, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := pendingApprovalKey{userID: req.UserID, agentID: req.AgentID, provider: req.Provider}
	items := c.pruneExpiredLocked(key, c.now().UTC())
	if len(items) == 0 {
		return nil, nil
	}
	index := -1
	if req.ApprovalID == "" {
		index = len(items) - 1
	} else {
		for i, pending := range items {
			if pending.ID == req.ApprovalID {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return nil, nil
	}
	if req.Stage != "" {
		items[index].Stage = req.Stage
	}
	if !req.ExpiresAt.IsZero() {
		items[index].ExpiresAt = req.ExpiresAt
	}
	c.pending[key] = items
	updated := items[index]
	return &updated, nil
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
