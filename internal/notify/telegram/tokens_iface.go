package telegram

import "time"

// CallbackTokenStorer abstracts storage of Telegram inline callback tokens
// so it can be backed by either in-memory state or Redis.
//
// taskID is the symmetric-dedup disambiguator for entryType=="approval".
// It is stored alongside the entry and surfaced via Consume so the consumer
// can route to the right pending row. Empty for all other entry types and
// for pre-task approvals.
type CallbackTokenStorer interface {
	Generate(entryType, targetID, userID, taskID, chatID string, ttl time.Duration) (approveID, denyID string, err error)
	Consume(shortID string) (*callbackEntry, error)
	Cleanup()
}

// Verify both implementations satisfy the interface.
var _ CallbackTokenStorer = (*callbackTokenStore)(nil)
