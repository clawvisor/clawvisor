package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	ticketExpiry = 30 * time.Second
	ticketBytes  = 32
)

// TicketStore manages single-use, short-lived tickets for SSE authentication.
// A user exchanges a valid JWT for a ticket, then uses the ticket as a query
// parameter when opening an EventSource (which cannot set headers).
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]*ticketEntry
}

type ticketEntry struct {
	UserID    string
	CreatedAt time.Time
	Used      bool
}

// NewTicketStore creates a new in-memory ticket store.
func NewTicketStore() *TicketStore {
	return &TicketStore{
		tickets: make(map[string]*ticketEntry),
	}
}

// Generate creates a new ticket for the given user.
func (s *TicketStore) Generate(userID string) (string, error) {
	b := make([]byte, ticketBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ticket := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[ticket] = &ticketEntry{
		UserID:    userID,
		CreatedAt: time.Now(),
	}
	return ticket, nil
}

// Validate checks a ticket and returns the associated user ID.
// The ticket is consumed (single-use) and cannot be reused.
func (s *TicketStore) Validate(ticket string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tickets[ticket]
	if !ok {
		return "", errors.New("ticket: not found")
	}
	if entry.Used {
		return "", errors.New("ticket: already used")
	}
	if time.Since(entry.CreatedAt) > ticketExpiry {
		delete(s.tickets, ticket)
		return "", errors.New("ticket: expired")
	}

	entry.Used = true
	return entry.UserID, nil
}

// Cleanup removes expired and used tickets from the store.
func (s *TicketStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for ticket, entry := range s.tickets {
		if entry.Used || time.Since(entry.CreatedAt) > ticketExpiry {
			delete(s.tickets, ticket)
		}
	}
}
