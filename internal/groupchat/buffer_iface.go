package groupchat

import "context"

// Buffer is the interface for group chat message buffering.
// Both in-memory (MessageBuffer) and Redis (RedisMessageBuffer) satisfy this.
type Buffer interface {
	Append(groupChatID string, msg BufferedMessage)
	Messages(groupChatID string) []BufferedMessage
	RunCleanup(ctx context.Context)
}
