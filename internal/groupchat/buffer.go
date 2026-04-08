// Package groupchat provides an in-memory buffer for recent messages from
// Telegram group chats. When a task is created, the buffer is consulted to
// determine if the user recently approved the work in conversation.
package groupchat

import (
	"context"
	"sync"
	"time"
)

// BufferedMessage is a single message captured from a monitored group chat.
type BufferedMessage struct {
	Text       string
	SenderID   string // Telegram user ID of the sender
	SenderName string // display name for LLM context
	Timestamp  time.Time
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
