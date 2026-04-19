// Package groupchat provides an in-memory buffer for recent messages from
// Telegram group chats. When a task is created, the buffer is consulted to
// determine if the user recently approved the work in conversation.
package groupchat

import (
	"context"
	"strings"
	"sync"
	"time"
)

// BufferedMessage is a single message captured from a monitored group
// chat. Most fields are optional provenance the plugin forwards so the
// approval LLM can render a structured transcript (clean delimiters,
// escapable JSON) rather than a flattened blob that user-controlled
// sender names could break out of.
type BufferedMessage struct {
	Text       string
	SenderID   string // platform-specific sender ID (e.g. Telegram user ID, Slack user ID)
	SenderName string // display name for LLM context
	Timestamp  time.Time

	// EventID is a client-generated idempotency key. Exact replays of the
	// same (bridge, event_id) are no-ops at the ingest boundary.
	EventID string
	// Seq is the per-bridge monotonic counter the ingest handler enforces
	// via AdvanceBridgeLastSeq. Protects against reorder and drop.
	Seq int64
	// BridgeID identifies which OpenClaw plugin install ingested this
	// message. Telegram-path entries leave it empty. Useful for audit and
	// for per-bridge filtering.
	BridgeID string
	// Channel is a human-meaningful platform tag ("slack", "discord",
	// "telegram", …). Optional; purely advisory for the LLM prompt.
	Channel string
	// ThreadID groups messages within a conversation (Slack thread_ts,
	// Discord thread id, Telegram message_thread_id). Optional.
	ThreadID string
	// MessageID is the platform's opaque per-message id. Optional; useful
	// for deduping across retries if the plugin doesn't generate its own
	// event_ids (it should, but this is defense in depth).
	MessageID string
	// Role is one of "user" | "assistant" | "tool" | "system". The LLM
	// uses this to distinguish human approval from agent chatter without
	// relying on name-matching heuristics.
	Role string
}

// UserScopedKey composes a buffer key that scopes a group chat ID to the
// owning user, so messages from different users cannot collide on shared
// group-chat identifiers (common for non-Telegram conversation IDs). All
// writers and readers must agree on this scheme; using the raw groupChatID
// in one place and this helper in another will silently break auto-approval.
func UserScopedKey(userID, groupChatID string) string {
	return "u:" + userID + ":" + groupChatID
}

// MessageBuffer stores recent group chat messages per group chat.
// Thread-safe; the polling loop appends and the task handler reads.
type MessageBuffer struct {
	mu       sync.Mutex
	messages map[string][]BufferedMessage // keyed by groupChatID
	maxCount int
	maxAge   time.Duration
}

// NewMessageBuffer creates a buffer that retains up to maxCount messages
// per group, discarding anything older than maxAge on read.
func NewMessageBuffer(maxCount int, maxAge time.Duration) *MessageBuffer {
	return &MessageBuffer{
		messages: make(map[string][]BufferedMessage),
		maxCount: maxCount,
		maxAge:   maxAge,
	}
}

// Append adds a message to the group's buffer. If the buffer exceeds maxCount,
// the oldest message is evicted.
func (b *MessageBuffer) Append(groupChatID string, msg BufferedMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := b.messages[groupChatID]
	msgs = append(msgs, msg)
	if len(msgs) > b.maxCount {
		msgs = msgs[len(msgs)-b.maxCount:]
	}
	b.messages[groupChatID] = msgs
}

// MessagesForKeyPrefix returns all non-stale messages across keys that
// start with `prefix`. Used by the debug-dump endpoint so a user can see
// what a bridge has buffered under its user scope (`u:{user_id}:`)
// without knowing every conversation key up front.
func (b *MessageBuffer) MessagesForKeyPrefix(prefix string) map[string][]BufferedMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := time.Now().Add(-b.maxAge)
	out := make(map[string][]BufferedMessage)
	for key, msgs := range b.messages {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var kept []BufferedMessage
		for _, m := range msgs {
			if m.Timestamp.After(cutoff) {
				kept = append(kept, m)
			}
		}
		if len(kept) > 0 {
			out[key] = kept
		}
	}
	return out
}

// Messages returns a copy of recent messages for the group, filtered to only
// include messages within maxAge. Messages are returned in chronological order
// (oldest first).
func (b *MessageBuffer) Messages(groupChatID string) []BufferedMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := b.messages[groupChatID]
	if len(msgs) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-b.maxAge)
	var result []BufferedMessage
	for _, m := range msgs {
		if m.Timestamp.After(cutoff) {
			result = append(result, m)
		}
	}
	return result
}

// RunCleanup periodically removes stale messages from the buffer.
func (b *MessageBuffer) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.cleanup()
		}
	}
}

func (b *MessageBuffer) cleanup() {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := time.Now().Add(-b.maxAge)
	for gid, msgs := range b.messages {
		var kept []BufferedMessage
		for _, m := range msgs {
			if m.Timestamp.After(cutoff) {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			delete(b.messages, gid)
		} else {
			b.messages[gid] = kept
		}
	}
}
