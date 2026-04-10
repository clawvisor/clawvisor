package handlers

import (
	"encoding/json"
	"sync"
	"time"
)

// DevicePairingStore abstracts device pairing session storage.
type DevicePairingStore interface {
	Store(token string, session *pairingSession)
	// LoadAndLock loads a session for atomic validation. Returns the session
	// and a function to call when done. The done function accepts whether
	// the session should be deleted.
	LoadAndLock(token string) (session *pairingSession, found bool, done func(delete bool))
	RunCleanup(done <-chan struct{})
}

// memoryDevicePairingStore is the default in-memory implementation.
type memoryDevicePairingStore struct {
	mu       sync.Mutex
	pairings map[string]*pairingSession
}

func newMemoryDevicePairingStore() *memoryDevicePairingStore {
	return &memoryDevicePairingStore{
		pairings: make(map[string]*pairingSession),
	}
}

func (s *memoryDevicePairingStore) Store(token string, session *pairingSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairings[token] = session
}

func (s *memoryDevicePairingStore) LoadAndLock(token string) (*pairingSession, bool, func(bool)) {
	s.mu.Lock()
	session, ok := s.pairings[token]
	if !ok {
		s.mu.Unlock()
		return nil, false, nil
	}
	return session, true, func(del bool) {
		if del {
			delete(s.pairings, token)
		}
		s.mu.Unlock()
	}
}

func (s *memoryDevicePairingStore) RunCleanup(done <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for token, session := range s.pairings {
				if now.After(session.ExpiresAt) {
					delete(s.pairings, token)
				}
			}
			s.mu.Unlock()
		}
	}
}

// pairingSessionJSON is the JSON-serializable form of pairingSession for Redis.
type pairingSessionJSON struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	Code      string    `json:"code"`
	Attempts  int       `json:"attempts"`
	ExpiresAt time.Time `json:"expires_at"`
}

func marshalPairingSession(s *pairingSession) ([]byte, error) {
	return json.Marshal(pairingSessionJSON{
		Token:     s.Token,
		UserID:    s.UserID,
		Code:      s.Code,
		Attempts:  s.Attempts,
		ExpiresAt: s.ExpiresAt,
	})
}

func unmarshalPairingSession(data []byte) (*pairingSession, error) {
	var j pairingSessionJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	return &pairingSession{
		Token:     j.Token,
		UserID:    j.UserID,
		Code:      j.Code,
		Attempts:  j.Attempts,
		ExpiresAt: j.ExpiresAt,
	}, nil
}
