package telegram

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	errTokenNotFound = errors.New("callback token not found")
	errTokenUsed     = errors.New("callback token already used")
	errTokenExpired  = errors.New("callback token expired")
)

// callbackEntry is one pair of approve/deny tokens for a single target.
type callbackEntry struct {
	Type      string // "approval", "task", "scope_expansion"
	TargetID  string
	UserID    string
	ChatID    string
	ExpiresAt time.Time
	Used      bool
}

// callbackTokenStore is an in-memory map of short callback IDs to entries.
// Each target gets two tokens (approve + deny) that share a single entry.
type callbackTokenStore struct {
	mu sync.Mutex
	// shortID → *callbackEntry
	tokens map[string]*callbackEntry
	// targetID → []*callbackEntry (so we can mark both tokens used)
	byTarget map[string][]string // targetID → []shortID
}

func newCallbackTokenStore() *callbackTokenStore {
	return &callbackTokenStore{
		tokens:   make(map[string]*callbackEntry),
		byTarget: make(map[string][]string),
	}
}

// Generate creates approve and deny tokens for a target.
// Returns (approveShortID, denyShortID, error).
func (s *callbackTokenStore) Generate(entryType, targetID, userID, chatID string, ttl time.Duration) (string, string, error) {
	approveID, err := randomShortID()
	if err != nil {
		return "", "", err
	}
	denyID, err := randomShortID()
	if err != nil {
		return "", "", err
	}

	entry := &callbackEntry{
		Type:      entryType,
		TargetID:  targetID,
		UserID:    userID,
		ChatID:    chatID,
		ExpiresAt: time.Now().Add(ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tokens[approveID] = entry
	s.tokens[denyID] = entry
	s.byTarget[targetID] = append(s.byTarget[targetID], approveID, denyID)

	return approveID, denyID, nil
}

// Consume validates a short ID and marks all tokens for the same target as used.
// Returns the entry on success (first-responder wins).
func (s *callbackTokenStore) Consume(shortID string) (*callbackEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tokens[shortID]
	if !ok {
		return nil, errTokenNotFound
	}
	if entry.Used {
		return nil, errTokenUsed
	}
	if time.Now().After(entry.ExpiresAt) {
		return nil, errTokenExpired
	}

	// Mark all tokens for this target as used (both approve and deny).
	entry.Used = true

	return entry, nil
}

// Cleanup removes expired and used entries.
func (s *callbackTokenStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for id, entry := range s.tokens {
		if entry.Used || now.After(entry.ExpiresAt) {
			delete(s.tokens, id)
		}
	}
	// Clean up byTarget index.
	for targetID, ids := range s.byTarget {
		remaining := ids[:0]
		for _, id := range ids {
			if _, ok := s.tokens[id]; ok {
				remaining = append(remaining, id)
			}
		}
		if len(remaining) == 0 {
			delete(s.byTarget, targetID)
		} else {
			s.byTarget[targetID] = remaining
		}
	}
}

// randomShortID generates a 16-byte random hex string (32 chars / 128 bits).
func randomShortID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
