package slack

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
//
// Slack's block_actions payload would let us round-trip the target ID
// directly in the button value (2000 chars, versus Telegram's 64-byte
// callback_data). We still mint opaque single-use tokens: the value comes
// back from the client, so a self-describing value would let anyone who can
// post an interaction replay a stale button or forge a target ID. The token
// is the authorization, the signature only proves Slack relayed it.
type callbackEntry struct {
	Type     string // "approval", "task", "scope_expansion", "connection"
	TargetID string
	// TaskID is the symmetric-dedup disambiguator for Type=="approval".
	TaskID string
	UserID string
	// ChannelID is the Slack channel the prompt was posted to. Consumed
	// decisions are checked against the live config so a token minted for a
	// channel the user has since disconnected cannot still resolve.
	ChannelID string
	ExpiresAt time.Time
	Used      bool
}

// CallbackTokenStorer mints and consumes the single-use tokens carried in
// Slack button values. The in-memory implementation below serves
// single-instance deployments; cloud uses the Redis implementation so a
// button click can land on any replica.
type CallbackTokenStorer interface {
	Generate(entryType, targetID, userID, taskID, channelID string, ttl time.Duration) (approveID, denyID string, err error)
	// Peek validates a token and returns its entry without retiring it, so
	// an unauthorized click cannot burn a live approval.
	Peek(shortID string) (*callbackEntry, error)
	Consume(shortID string) (*callbackEntry, error)
	Cleanup()
}

// callbackTokenStore is an in-memory map of short callback IDs to entries.
// Each target gets two tokens (approve + deny) that share a single entry, so
// consuming either one retires both — first responder wins.
type callbackTokenStore struct {
	mu       sync.Mutex
	tokens   map[string]*callbackEntry
	byTarget map[string][]string // targetID → []shortID
}

func newCallbackTokenStore() *callbackTokenStore {
	return &callbackTokenStore{
		tokens:   make(map[string]*callbackEntry),
		byTarget: make(map[string][]string),
	}
}

// Generate creates approve and deny tokens for a target.
func (s *callbackTokenStore) Generate(entryType, targetID, userID, taskID, channelID string, ttl time.Duration) (string, string, error) {
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
		TaskID:    taskID,
		UserID:    userID,
		ChannelID: channelID,
		ExpiresAt: time.Now().Add(ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tokens[approveID] = entry
	s.tokens[denyID] = entry
	s.byTarget[targetID] = append(s.byTarget[targetID], approveID, denyID)

	return approveID, denyID, nil
}

// Peek validates a short ID and returns a copy of its entry, leaving the
// token live. Callers must still Consume before acting on the decision.
func (s *callbackTokenStore) Peek(shortID string) (*callbackEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.lookupLocked(shortID)
	if err != nil {
		return nil, err
	}
	cp := *entry
	return &cp, nil
}

// lookupLocked resolves and validates a token. Callers must hold s.mu.
func (s *callbackTokenStore) lookupLocked(shortID string) (*callbackEntry, error) {
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
	return entry, nil
}

// Consume validates a short ID and retires every token for the same target.
func (s *callbackTokenStore) Consume(shortID string) (*callbackEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.lookupLocked(shortID)
	if err != nil {
		return nil, err
	}
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

// randomShortID generates a 16-byte random hex string (128 bits).
func randomShortID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
