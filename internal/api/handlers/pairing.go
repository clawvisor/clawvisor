package handlers

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/relay"
)

const (
	pairingCodeExpiry      = 5 * time.Minute
	maxPairingCodeAttempts = 3
)

// PairingHandler manages the daemon-side pairing code flow used by the relay's
// MCP OAuth consent page. Only one active code at a time.
type PairingHandler struct {
	daemonID string
	store    PairingCodeStore
}

// NewPairingHandler creates a PairingHandler for the given daemon ID.
func NewPairingHandler(daemonID string) *PairingHandler {
	return &PairingHandler{
		daemonID: daemonID,
		store:    newMemoryPairingCodeStore(pairingCodeExpiry, maxPairingCodeAttempts),
	}
}

// SetPairingCodeStore overrides the default in-memory pairing code store.
func (h *PairingHandler) SetPairingCodeStore(s PairingCodeStore) {
	h.store = s
}

// GenerateCode handles GET /api/pairing/code (no auth — localhost is the security boundary).
// Generates a new 6-digit code, invalidating any previous one.
// Rejects requests that arrive through the relay tunnel.
func (h *PairingHandler) GenerateCode(w http.ResponseWriter, r *http.Request) {
	if relay.ViaRelay(r.Context()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if h.daemonID == "" {
		writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "relay is not configured — run clawvisor setup")
		return
	}

	code, err := generatePairingCode6()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate pairing code")
		return
	}
	h.store.Set(code)

	writeJSON(w, http.StatusOK, map[string]any{
		"daemon_id":  h.daemonID,
		"code":       code,
		"expires_in": int(pairingCodeExpiry.Seconds()),
	})
}

// Verify validates and consumes the pairing code (single-use). Limited to 3
// attempts per code to prevent brute-force guessing. Returns true if the code
// is valid and was consumed, false otherwise.
func (h *PairingHandler) Verify(code string) bool {
	return h.store.Verify(code)
}

func generatePairingCode6() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
