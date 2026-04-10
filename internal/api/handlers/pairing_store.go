package handlers

import (
	"sync"
	"time"
)

// PairingCodeStore abstracts the single-code MCP pairing flow so it can be
// backed by either in-memory state or Redis.
type PairingCodeStore interface {
	// Set stores a new pairing code, replacing any existing one.
	Set(code string)
	// Verify validates and consumes the code (single-use). Returns true on success.
	Verify(code string) bool
}

// memoryPairingCodeStore is the default in-memory implementation.
type memoryPairingCodeStore struct {
	mu       sync.Mutex
	code     string
	created  time.Time
	attempts int
	expiry   time.Duration
	maxAttempts int
}

func newMemoryPairingCodeStore(expiry time.Duration, maxAttempts int) *memoryPairingCodeStore {
	return &memoryPairingCodeStore{expiry: expiry, maxAttempts: maxAttempts}
}

func (s *memoryPairingCodeStore) Set(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
	s.created = time.Now()
	s.attempts = 0
}

func (s *memoryPairingCodeStore) Verify(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.code == "" || time.Since(s.created) > s.expiry {
		s.code = ""
		return false
	}
	if s.code != code {
		s.attempts++
		if s.attempts >= s.maxAttempts {
			s.code = ""
		}
		return false
	}
	s.code = ""
	return true
}
