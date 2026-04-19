package groupchat

import "context"

// Buffer is the interface for group chat message buffering.
// Both in-memory (MessageBuffer) and Redis (RedisMessageBuffer) satisfy this.
type Buffer interface {
	Append(groupChatID string, msg BufferedMessage)
	Messages(groupChatID string) []BufferedMessage
	RunCleanup(ctx context.Context)
	// MessagesForKeyPrefix enumerates all message lists whose key starts
	// with the given prefix. Used by the debug-dump endpoint so the
	// Agents page can show what's actually buffered under a bridge's
	// user scope (`u:{user_id}:`). Returns map[groupChatID] → messages
	// filtered by maxAge (same filter Messages() applies).
	MessagesForKeyPrefix(prefix string) map[string][]BufferedMessage
}
