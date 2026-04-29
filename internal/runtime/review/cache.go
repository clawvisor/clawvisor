package review

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"sync"
	"time"
)

// HeldApproval is one buffered tool_use awaiting a user verdict through an
// inline approval surface.
type HeldApproval struct {
	ID               string
	ApprovalRecordID string
	TaskID           string
	ToolUseID        string
	ToolName         string
	ToolInput        map[string]any
	Reason           string
	CreatedAt        time.Time
}

// ApprovalCache holds buffered tool_use approvals per runtime session.
type ApprovalCache struct {
	IdleTTL time.Duration

	mu       sync.Mutex
	sessions map[string][]*HeldApproval
	nowFn    func() time.Time
}

func NewApprovalCache() *ApprovalCache {
	return &ApprovalCache{
		IdleTTL:  30 * time.Minute,
		sessions: make(map[string][]*HeldApproval),
		nowFn:    time.Now,
	}
}

func (c *ApprovalCache) Hold(sessionID, approvalRecordID, taskID, toolUseID, toolName string, toolInput map[string]any, reason string) (*HeldApproval, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.activeLocked(sessionID)
	for _, existing := range active {
		if existing.ApprovalRecordID == approvalRecordID {
			return existing, false
		}
	}
	h := &HeldApproval{
		ID:               mintApprovalID(),
		ApprovalRecordID: approvalRecordID,
		TaskID:           taskID,
		ToolUseID:        toolUseID,
		ToolName:         toolName,
		ToolInput:        toolInput,
		Reason:           reason,
		CreatedAt:        c.nowFn(),
	}
	c.sessions[sessionID] = append(active, h)
	return h, true
}

func (c *ApprovalCache) Get(sessionID string) *HeldApproval {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.activeLocked(sessionID)
	if len(active) == 0 {
		return nil
	}
	return active[0]
}

func (c *ApprovalCache) GetByApprovalRecord(sessionID, approvalRecordID string) *HeldApproval {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, held := range c.activeLocked(sessionID) {
		if held.ApprovalRecordID == approvalRecordID {
			return held
		}
	}
	return nil
}

func (c *ApprovalCache) Resolve(sessionID, id string) *HeldApproval {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.activeLocked(sessionID)
	for i, held := range active {
		if held.ID != id {
			continue
		}
		active = append(active[:i], active[i+1:]...)
		if len(active) == 0 {
			delete(c.sessions, sessionID)
		} else {
			c.sessions[sessionID] = active
		}
		return held
	}
	return nil
}

func (c *ApprovalCache) Drop(sessionID string, ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(ids) == 0 {
		delete(c.sessions, sessionID)
		return
	}
	active := c.activeLocked(sessionID)
	if len(active) == 0 {
		return
	}
	drop := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	filtered := active[:0]
	for _, held := range active {
		if _, ok := drop[held.ID]; ok {
			continue
		}
		filtered = append(filtered, held)
	}
	if len(filtered) == 0 {
		delete(c.sessions, sessionID)
		return
	}
	c.sessions[sessionID] = filtered
}

func (c *ApprovalCache) RebindTask(sessionID, approvalRecordID, taskID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, held := range c.activeLocked(sessionID) {
		if held.ApprovalRecordID != approvalRecordID {
			continue
		}
		held.TaskID = taskID
		return true
	}
	return false
}

func (c *ApprovalCache) Count(sessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.activeLocked(sessionID))
}

func (c *ApprovalCache) List(sessionID string) []*HeldApproval {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.activeLocked(sessionID)
	out := make([]*HeldApproval, len(active))
	copy(out, active)
	return out
}

func (c *ApprovalCache) activeLocked(sessionID string) []*HeldApproval {
	held, ok := c.sessions[sessionID]
	if !ok {
		return nil
	}
	if c.IdleTTL <= 0 {
		return held
	}
	now := c.nowFn()
	filtered := held[:0]
	for _, h := range held {
		if now.Sub(h.CreatedAt) <= c.IdleTTL {
			filtered = append(filtered, h)
		}
	}
	if len(filtered) == 0 {
		delete(c.sessions, sessionID)
		return nil
	}
	if len(filtered) != len(held) {
		c.sessions[sessionID] = filtered
	}
	return filtered
}

func mintApprovalID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return "cv-" + strings.ToLower(enc[:12])
}
