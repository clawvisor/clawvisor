package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/notify/push"
	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	pairingExpiry     = 5 * time.Minute
	maxPairingAttempts = 3
)

// DevicesHandler manages device pairing and action endpoints.
type DevicesHandler struct {
	st       store.Store
	pushN    *push.Notifier // may be nil if push is not configured
	logger   *slog.Logger
	baseURL  string
	jwtSvc   pkgauth.TokenService

	pairingsMu sync.Mutex
	pairings   map[string]*pairingSession
}

type pairingSession struct {
	Token     string
	UserID    string
	Code      string // 6-digit numeric code
	Attempts  int
	ExpiresAt time.Time
}

func NewDevicesHandler(st store.Store, pushN *push.Notifier, logger *slog.Logger, baseURL string, jwtSvc pkgauth.TokenService) *DevicesHandler {
	return &DevicesHandler{
		st:       st,
		pushN:    pushN,
		logger:   logger,
		baseURL:  baseURL,
		jwtSvc:   jwtSvc,
		pairings: make(map[string]*pairingSession),
	}
}

// StartPairing handles POST /api/devices/pair (user JWT).
// Returns a pairing token and 6-digit code for the mobile App Clip to use.
func (h *DevicesHandler) StartPairing(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	token, err := generatePairingToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate pairing token")
		return
	}

	code, err := generatePairingCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate pairing code")
		return
	}

	session := &pairingSession{
		Token:     token,
		UserID:    user.ID,
		Code:      code,
		ExpiresAt: time.Now().Add(pairingExpiry),
	}
	h.pairingsMu.Lock()
	h.pairings[token] = session
	h.pairingsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"pairing_token": token,
		"code":          code,
		"pairing_url":   h.baseURL + "/api/devices/pair/complete",
		"expires_at":    session.ExpiresAt,
	})
}

// CompletePairing handles POST /api/devices/pair/complete (unauthenticated, rate-limited).
// The mobile app presents the pairing token, code, device name, and push token.
func (h *DevicesHandler) CompletePairing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PairingToken string `json:"pairing_token"`
		Code         string `json:"code"`
		DeviceName   string `json:"device_name"`
		DeviceToken  string `json:"device_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.PairingToken == "" || body.Code == "" || body.DeviceName == "" || body.DeviceToken == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "pairing_token, code, device_name, and device_token are required")
		return
	}

	// Hold the mutex for the entire validate-and-consume sequence to prevent
	// concurrent requests from racing past the attempt limit or both succeeding
	// with the correct code.
	h.pairingsMu.Lock()
	session, ok := h.pairings[body.PairingToken]
	if !ok {
		h.pairingsMu.Unlock()
		writeError(w, http.StatusNotFound, "NOT_FOUND", "pairing session not found or expired")
		return
	}

	if time.Now().After(session.ExpiresAt) {
		delete(h.pairings, body.PairingToken)
		h.pairingsMu.Unlock()
		writeError(w, http.StatusGone, "EXPIRED", "pairing session has expired")
		return
	}

	if session.Code != body.Code {
		session.Attempts++
		if session.Attempts >= maxPairingAttempts {
			delete(h.pairings, body.PairingToken)
			h.pairingsMu.Unlock()
			writeError(w, http.StatusUnauthorized, "MAX_ATTEMPTS", "too many incorrect code attempts")
			return
		}
		h.pairingsMu.Unlock()
		writeError(w, http.StatusUnauthorized, "INVALID_CODE", "incorrect pairing code")
		return
	}

	// Code is correct — remove the session before releasing the lock.
	delete(h.pairings, body.PairingToken)
	h.pairingsMu.Unlock()

	// Generate HMAC key (32 random bytes, hex-encoded).
	hmacKey := make([]byte, 32)
	if _, err := rand.Read(hmacKey); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate HMAC key")
		return
	}
	hmacKeyHex := hex.EncodeToString(hmacKey)

	device := &store.PairedDevice{
		UserID:        session.UserID,
		DeviceName:    body.DeviceName,
		DeviceToken:   body.DeviceToken,
		DeviceHMACKey: hmacKeyHex,
	}
	if err := h.st.CreatePairedDevice(r.Context(), device); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create paired device")
		return
	}

	// Register with push service if available.
	if h.pushN != nil {
		if err := h.pushN.RegisterDevice(r.Context(), body.DeviceToken); err != nil {
			h.logger.Warn("failed to register device with push service", "err", err)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"device_id": device.ID,
		"hmac_key":  hmacKeyHex,
	})
}

// List handles GET /api/devices (user JWT).
func (h *DevicesHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	devices, err := h.st.ListPairedDevices(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list devices")
		return
	}
	if devices == nil {
		devices = []*store.PairedDevice{}
	}
	writeJSON(w, http.StatusOK, devices)
}

// Delete handles DELETE /api/devices/{id} (user JWT).
func (h *DevicesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	device, err := h.st.GetPairedDevice(r.Context(), id)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "device not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get device")
		return
	}
	if device.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your device")
		return
	}

	// Deregister from push service before deleting.
	if h.pushN != nil {
		if err := h.pushN.DeregisterDevice(r.Context(), device.DeviceToken); err != nil {
			h.logger.Warn("failed to deregister device from push service", "err", err)
		}
	}

	if err := h.st.DeletePairedDevice(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete device")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// Action handles POST /api/devices/{id}/action (DeviceHMAC auth).
// The mobile app sends approve/deny decisions via this endpoint.
func (h *DevicesHandler) Action(w http.ResponseWriter, r *http.Request) {
	device := middleware.DeviceFromContext(r.Context())
	if device == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "device not authenticated")
		return
	}

	var body struct {
		Action      string `json:"action"`       // "approve" or "deny"
		TargetID    string `json:"target_id"`     // request/task/connection ID
		RequestType string `json:"request_type"`  // "approval", "task", "scope_expansion", "connection"
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Action == "" || body.TargetID == "" || body.RequestType == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "action, target_id, and request_type are required")
		return
	}
	if body.Action != "approve" && body.Action != "deny" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "action must be 'approve' or 'deny'")
		return
	}

	// Update last seen.
	_ = h.st.UpdatePairedDeviceLastSeen(r.Context(), device.ID)

	// Emit decision through the push notifier's channel.
	if h.pushN == nil {
		writeError(w, http.StatusServiceUnavailable, "PUSH_NOT_CONFIGURED", "push notification service is not configured")
		return
	}
	h.pushN.EmitDecision(notify.CallbackDecision{
		Type:     body.RequestType,
		Action:   body.Action,
		TargetID: body.TargetID,
		UserID:   device.UserID,
	})

	writeJSON(w, http.StatusOK, map[string]any{"status": "received"})
}

// RunCleanup periodically sweeps expired pairing sessions.
func (h *DevicesHandler) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			h.pairingsMu.Lock()
			for token, session := range h.pairings {
				if now.After(session.ExpiresAt) {
					delete(h.pairings, token)
				}
			}
			h.pairingsMu.Unlock()
		}
	}
}

// generatePairingToken returns a 32-byte URL-safe base64 token.
func generatePairingToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

// MintToken handles POST /api/devices/{id}/token (DeviceHMAC auth).
// Returns a short-lived User JWT for the device's linked user.
func (h *DevicesHandler) MintToken(w http.ResponseWriter, r *http.Request) {
	device := middleware.DeviceFromContext(r.Context())
	if device == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "device not authenticated")
		return
	}

	// Defense in depth: verify path param matches authenticated device.
	if r.PathValue("id") != device.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "device ID mismatch")
		return
	}

	// Look up the user to get their email for the JWT claims.
	user, err := h.st.GetUserByID(r.Context(), device.UserID)
	if err != nil {
		h.logger.Error("mint device token: user lookup failed", "device_id", device.ID, "user_id", device.UserID, "error", err)
		writeError(w, http.StatusInternalServerError, "TOKEN_ERROR", "failed to resolve user")
		return
	}

	const tokenTTL = 5 * time.Minute
	token, err := h.jwtSvc.GenerateAccessToken(user.ID, user.Email, tokenTTL)
	if err != nil {
		h.logger.Error("mint device token failed", "device_id", device.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "TOKEN_ERROR", "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"expires_in":   int(tokenTTL.Seconds()),
	})
}

// generatePairingCode returns a random 6-digit numeric code.
func generatePairingCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
