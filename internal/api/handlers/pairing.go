package handlers

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"sync"
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

	mu       sync.Mutex
	code     string
	created  time.Time
	attempts int
}

// NewPairingHandler creates a PairingHandler for the given daemon ID.
func NewPairingHandler(daemonID string) *PairingHandler {
	return &PairingHandler{daemonID: daemonID}
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
		writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "relay is not configured — run clawvisor daemon setup")
		return
	}

	h.mu.Lock()
	code, err := generatePairingCode6()
	if err != nil {
		h.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate pairing code")
		return
	}
	h.code = code
	h.created = time.Now()
	h.attempts = 0
	h.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"daemon_id":  h.daemonID,
		"code":       code,
		"expires_in": int(pairingCodeExpiry.Seconds()),
	})
}

// VerifyCode handles POST /api/pairing/verify (no auth — only reachable via tunnel).
// Validates and consumes the pairing code (single-use). Limited to 3 attempts
// per code to prevent brute-force guessing.
func (h *PairingHandler) VerifyCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"pairing_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "code is required")
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// No active code or expired.
	if h.code == "" || time.Since(h.created) > pairingCodeExpiry {
		h.code = ""
		writeJSON(w, http.StatusOK, map[string]any{"valid": false})
		return
	}

	// Wrong code — count the attempt.
	if h.code != body.Code {
		h.attempts++
		if h.attempts >= maxPairingCodeAttempts {
			h.code = "" // burn the code after max attempts
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": false})
		return
	}

	// Correct — consume the code.
	h.code = ""
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func generatePairingCode6() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
