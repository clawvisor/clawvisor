package events

import "sync"

// Event represents an SSE notification sent to dashboard clients or
// the Clawvisor Proxy. For dashboard events Type is "queue"/"tasks"/
// "audit" with an optional ID. For proxy-facing events (see
// CredentialHandler.Invalidations), Type is "credential.invalidate"
// and Data carries {cache_key, credential_ref}.
type Event struct {
	Type string            `json:"type"`
	ID   string            `json:"id,omitempty"`
	Data map[string]string `json:"data,omitempty"`
}

// EventHub is the interface for event fan-out to SSE connections and long-poll waiters.
// The in-memory Hub satisfies this interface; cloud deployments may use a Redis-backed implementation.
type EventHub interface {
	Subscribe(userID string) (<-chan Event, func())
	Publish(userID string, e Event)
	Close()
}

// Hub fans out events to per-user SSE connections.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]map[chan Event]struct{} // userID -> set of channels
}

// NewHub creates a new event hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[string]map[chan Event]struct{}),
	}
}

// Subscribe registers a new SSE client for the given user.
// Returns an event channel and an unsubscribe function.
func (h *Hub) Subscribe(userID string) (<-chan Event, func()) {
	ch := make(chan Event, 16)

	h.mu.Lock()
	if h.clients[userID] == nil {
		h.clients[userID] = make(map[chan Event]struct{})
	}
	h.clients[userID][ch] = struct{}{}
	h.mu.Unlock()

	unsub := func() {
		h.mu.Lock()
		// Guard against double-close: if Close() already removed and
		// closed this channel, it won't be in the map.
		if _, exists := h.clients[userID][ch]; exists {
			delete(h.clients[userID], ch)
			if len(h.clients[userID]) == 0 {
				delete(h.clients, userID)
			}
			close(ch)
		}
		h.mu.Unlock()
		// Drain any buffered events.
		for range ch {
		}
	}

	return ch, unsub
}

// Publish sends an event to all SSE connections for the given user.
// Slow clients whose buffer is full are skipped (non-blocking send).
func (h *Hub) Publish(userID string, e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients[userID] {
		select {
		case ch <- e:
		default:
			// Client buffer full — drop event; polling will catch up.
		}
	}
}

// Close closes all client channels, unblocking any SSE handlers waiting
// in their select loops. Must be called before http.Server.Shutdown so
// handlers return promptly.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for userID, clients := range h.clients {
		for ch := range clients {
			close(ch)
		}
		delete(h.clients, userID)
	}
}
