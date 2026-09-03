package slack

import (
	"strings"
	"sync"
	"time"
)

// messageContext is what the resolve path needs but is not carried on
// notify.TargetMessageUpdater's arguments: a one-line summary of the request,
// the detail blocks originally posted, and who resolved it.
//
// Without this, resolving replaces the whole message with "✅ Approved" and
// the record of what was approved is gone — the wrong outcome for a channel
// people use as an audit trail.
type messageContext struct {
	// Summary is a compact description kept on the resolved message so the
	// channel still says what was approved without expanding the thread.
	Summary string
	// Detail is the block set originally posted, minus the button row. On
	// resolve it moves into a thread reply, which is Slack's only real
	// collapse primitive.
	Detail []block
	// Approver is a Slack mention (`<@U012ABC>`) for whoever clicked, set
	// at interaction time. Empty when the request was resolved from the
	// dashboard or by expiry, in which case the resolved message simply
	// omits attribution rather than guessing.
	Approver  string
	resolved  bool
	expiresAt time.Time
}

// MessageContextStorer holds per-target message context between sending a
// prompt and resolving it.
//
// The in-memory implementation is correct for single-instance deployments.
// Across replicas a decision can be consumed on a different instance than
// the one that posted or received the click, in which case the lookup misses
// and the resolved message degrades gracefully — it keeps the outcome and
// drops the summary, thread reply, and attribution. A Redis-backed
// implementation would close that gap.
type MessageContextStorer interface {
	Put(key string, mc messageContext, ttl time.Duration)
	SetApprover(key, approver string)
	// TakeForResolve returns the context and marks it resolved. The second
	// return is false if it is absent or already resolved, so a duplicate
	// resolution cannot post the detail thread twice.
	TakeForResolve(key string) (messageContext, bool)
	Cleanup()
}

type memoryMessageContextStore struct {
	mu sync.Mutex
	m  map[string]*messageContext
}

func newMessageContextStore() *memoryMessageContextStore {
	return &memoryMessageContextStore{m: make(map[string]*messageContext)}
}

func (s *memoryMessageContextStore) Put(key string, mc messageContext, ttl time.Duration) {
	mc.expiresAt = time.Now().Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = &mc
}

func (s *memoryMessageContextStore) SetApprover(key, approver string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mc, ok := s.m[key]; ok {
		mc.Approver = approver
	}
}

func (s *memoryMessageContextStore) TakeForResolve(key string) (messageContext, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mc, ok := s.m[key]
	if !ok || mc.resolved || time.Now().After(mc.expiresAt) {
		return messageContext{}, false
	}
	mc.resolved = true
	return *mc, true
}

func (s *memoryMessageContextStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, mc := range s.m {
		if mc.resolved || now.After(mc.expiresAt) {
			delete(s.m, k)
		}
	}
}

// contextKey addresses a target the same way notification_messages does, so
// the send and resolve paths agree.
func contextKey(targetType, targetID string) string {
	return targetType + ":" + targetID
}

// targetTypeForDecision maps a callback entry type onto the target type used
// in notification_messages. Scope expansion resolves against its parent task,
// so it shares the "task" namespace.
func targetTypeForDecision(entryType string) string {
	switch entryType {
	case "task", "scope_expansion":
		return "task"
	case "connection":
		return "connection"
	default:
		return "approval"
	}
}

// mention renders a Slack user ID as a clickable mention. Slack resolves
// `<@U012ABC>` to the member's display name at render time, so it stays
// correct if they change their name.
func mention(slackUserID string) string {
	if slackUserID == "" {
		return ""
	}
	return "<@" + slackUserID + ">"
}

// summarise builds the compact one-liner kept on a resolved message.
func summarise(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}
