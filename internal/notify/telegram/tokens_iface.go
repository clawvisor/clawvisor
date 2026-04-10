package telegram

import "time"

// CallbackTokenStorer abstracts storage of Telegram inline callback tokens
// so it can be backed by either in-memory state or Redis.
type CallbackTokenStorer interface {
	Generate(entryType, targetID, userID, chatID string, ttl time.Duration) (approveID, denyID string, err error)
	Consume(shortID string) (*callbackEntry, error)
	Cleanup()
}

// Verify both implementations satisfy the interface.
var _ CallbackTokenStorer = (*callbackTokenStore)(nil)
