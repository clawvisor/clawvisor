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

type callbackEntry struct {
	Type      string
	TargetID  string
	TaskID    string
	UserID    string
	ChannelID string
	ExpiresAt time.Time
	Used      bool
}

type CallbackTokenStore interface {
	Generate(entryType, targetID, userID, taskID, channelID string, ttl time.Duration) (approveID, denyID string, err error)
	Consume(shortID string) (*callbackEntry, error)
	Cleanup()
}

type callbackTokenStore struct {
	mu       sync.Mutex
	tokens   map[string]*callbackEntry
	byTarget map[string][]string
}

func newCallbackTokenStore() *callbackTokenStore {
	return &callbackTokenStore{
		tokens:   make(map[string]*callbackEntry),
		byTarget: make(map[string][]string),
	}
}

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
	entry.Used = true
	return entry, nil
}

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

func randomShortID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var _ CallbackTokenStore = (*callbackTokenStore)(nil)
