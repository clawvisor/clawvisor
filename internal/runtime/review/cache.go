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
	ID        string
	ToolUseID string
	ToolName  string
	ToolInput map[string]any
	Reason    string
	CreatedAt time.Time
}

// ApprovalCache holds one active held approval per runtime session.
type ApprovalCache struct {
	IdleTTL time.Duration

	mu       sync.Mutex
	sessions map[string]*HeldApproval
	nowFn    func() time.Time
}

func NewApprovalCache() *ApprovalCache {
	return &ApprovalCache{
		IdleTTL:  30 * time.Minute,
		sessions: make(map[string]*HeldApproval),
		nowFn:    time.Now,
	}
}

func (c *ApprovalCache) Hold(sessionID, toolUseID, toolName string, toolInput map[string]any, reason string) (*HeldApproval, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.activeLocked(sessionID); existing != nil {
		return nil, false
	}
	h := &HeldApproval{
		ID:        mintApprovalID(),
		ToolUseID: toolUseID,
		ToolName:  toolName,
		ToolInput: toolInput,
		Reason:    reason,
		CreatedAt: c.nowFn(),
	}
	c.sessions[sessionID] = h
	return h, true
}

func (c *ApprovalCache) Get(sessionID string) *HeldApproval {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeLocked(sessionID)
}

func (c *ApprovalCache) Resolve(sessionID, id string) *HeldApproval {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.activeLocked(sessionID)
	if h == nil || h.ID != id {
		return nil
	}
	delete(c.sessions, sessionID)
	return h
}

func (c *ApprovalCache) Drop(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, sessionID)
}

func (c *ApprovalCache) activeLocked(sessionID string) *HeldApproval {
	h, ok := c.sessions[sessionID]
	if !ok {
		return nil
	}
	if c.IdleTTL > 0 && c.nowFn().Sub(h.CreatedAt) > c.IdleTTL {
		delete(c.sessions, sessionID)
		return nil
	}
	return h
}

func mintApprovalID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return "cv-" + strings.ToLower(enc[:12])
}
