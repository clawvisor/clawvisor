package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

var (
	errAlreadyResolved = errors.New("connection request already resolved")
	errExpired         = errors.New("connection request expired")
	errForbidden       = errors.New("connection request does not belong to this user")
)

const (
	connectionRequestExpiry   = 5 * time.Minute
	connectionTokenWindow     = 5 * time.Minute
	maxPendingRequests        = 10
	pollTimeout               = 30 * time.Second
	maxConcurrentPolls  int64 = 50
)

// ConnectionsHandler manages agent connection request lifecycle.
type ConnectionsHandler struct {
	st       store.Store
	notifier notify.Notifier
	eventHub *events.Hub
	logger   *slog.Logger

	// In-memory token cache: connection request ID → {raw token, approved time}.
	// Tokens are never persisted; lost on daemon restart.
	tokensMu sync.RWMutex
	tokens   map[string]approvedToken

	// Active long-poll count.
	activePolls atomic.Int64

	// Per-IP concurrent poll tracking.
	ipPollsMu sync.Mutex
	ipPolls   map[string]int
}

type approvedToken struct {
	raw        string
	approvedAt time.Time
}

func NewConnectionsHandler(st store.Store, notifier notify.Notifier,
	eventHub *events.Hub, logger *slog.Logger) *ConnectionsHandler {
	return &ConnectionsHandler{
		st:       st,
		notifier: notifier,
		eventHub: eventHub,
		logger:   logger,
		tokens:   make(map[string]approvedToken),
		ipPolls:  make(map[string]int),
	}
}

// RequestConnect handles POST /api/agents/connect (unauthenticated).
// An agent calls this to request access to the daemon.
func (h *ConnectionsHandler) RequestConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		CallbackURL string `json:"callback_url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	// Check pending count.
	count, err := h.st.CountPendingConnectionRequests(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not check pending requests")
		return
	}
	if count >= maxPendingRequests {
		writeError(w, http.StatusTooManyRequests, "TOO_MANY_PENDING", "too many pending connection requests")
		return
	}

	// Resolve daemon owner — single-user daemon uses admin@local.
	owner, err := h.st.GetUserByEmail(r.Context(), "admin@local")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not resolve daemon owner")
		return
	}

	req := &store.ConnectionRequest{
		UserID:      owner.ID,
		Name:        body.Name,
		Description: body.Description,
		CallbackURL: body.CallbackURL,
		Status:      "pending",
		IPAddress:   r.RemoteAddr,
		ExpiresAt:   time.Now().Add(connectionRequestExpiry),
	}
	if err := h.st.CreateConnectionRequest(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create connection request")
		return
	}

	// Notify owner via SSE and push notification.
	h.eventHub.Publish(owner.ID, events.Event{Type: "queue"})
	if h.notifier != nil {
		msg := fmt.Sprintf("Agent %q is requesting to connect from %s", body.Name, r.RemoteAddr)
		if err := h.notifier.SendAlert(r.Context(), owner.ID, msg); err != nil {
			h.logger.Warn("failed to send connection request notification", "err", err)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"connection_id": req.ID,
		"status":        req.Status,
		"poll_url":      "/api/agents/connect/" + req.ID + "/status",
		"expires_at":    req.ExpiresAt,
	})
}

// PollStatus handles GET /api/agents/connect/{id}/status (unauthenticated).
// Long-polls until the connection request is resolved or timeout.
func (h *ConnectionsHandler) PollStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Check and return current status. If not pending, return immediately.
	respond := func() (done bool) {
		cr, err := h.st.GetConnectionRequest(r.Context(), id)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "connection request not found")
			} else {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get connection request")
			}
			return true
		}

		// Check expiry.
		if cr.Status == "pending" && time.Now().After(cr.ExpiresAt) {
			_ = h.st.UpdateConnectionRequestStatus(r.Context(), id, "expired", "")
			cr.Status = "expired"
		}

		if cr.Status == "pending" {
			return false
		}

		resp := map[string]any{"status": cr.Status}
		if cr.Status == "approved" {
			h.tokensMu.RLock()
			tok, ok := h.tokens[id]
			h.tokensMu.RUnlock()
			if ok && time.Since(tok.approvedAt) < connectionTokenWindow {
				resp["token"] = tok.raw
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return true
	}

	// First check — return immediately if resolved.
	if respond() {
		return
	}

	// Per-IP concurrent poll limit (max 3).
	ip := r.RemoteAddr
	h.ipPollsMu.Lock()
	if h.ipPolls[ip] >= 3 {
		h.ipPollsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	h.ipPolls[ip]++
	h.ipPollsMu.Unlock()
	defer func() {
		h.ipPollsMu.Lock()
		h.ipPolls[ip]--
		if h.ipPolls[ip] <= 0 {
			delete(h.ipPolls, ip)
		}
		h.ipPollsMu.Unlock()
	}()

	// Global concurrent poll limit.
	active := h.activePolls.Add(1)
	defer h.activePolls.Add(-1)

	if active > maxConcurrentPolls {
		// Degrade: return current status immediately.
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}

	// Look up the connection request to get the owner's user ID for SSE subscription.
	cr, err := h.st.GetConnectionRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}

	ch, unsub := h.eventHub.Subscribe(cr.UserID)
	defer unsub()

	timer := time.NewTimer(pollTimeout)
	defer timer.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			// Timeout — return current status, or pending if still unresolved.
			if !respond() {
				writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			}
			return
		case _, ok := <-ch:
			if !ok {
				respond()
				return
			}
			if respond() {
				return
			}
		}
	}
}

// Approve handles POST /api/agents/connect/{id}/approve (user JWT).
func (h *ConnectionsHandler) Approve(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	agentID, err := h.ApproveByID(r.Context(), id, user.ID)
	if err != nil {
		switch err {
		case store.ErrNotFound:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "connection request not found")
		case errForbidden:
			writeError(w, http.StatusForbidden, "FORBIDDEN", "not your connection request")
		case errAlreadyResolved:
			writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "connection request is not pending")
		case errExpired:
			writeError(w, http.StatusGone, "EXPIRED", "connection request has expired")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve connection request")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "approved",
		"agent_id": agentID,
	})
}

// ApproveByID is the core approve logic, callable from HTTP handlers and
// the notifier decision consumer.
func (h *ConnectionsHandler) ApproveByID(ctx context.Context, id, userID string) (agentID string, err error) {
	cr, err := h.st.GetConnectionRequest(ctx, id)
	if err != nil {
		return "", err
	}
	if cr.UserID != userID {
		return "", errForbidden
	}
	if cr.Status != "pending" {
		return "", errAlreadyResolved
	}
	if time.Now().After(cr.ExpiresAt) {
		_ = h.st.UpdateConnectionRequestStatus(ctx, id, "expired", "")
		return "", errExpired
	}

	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	agent, err := h.st.CreateAgent(ctx, userID, cr.Name, auth.HashToken(rawToken))
	if err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}

	if err := h.st.UpdateConnectionRequestStatus(ctx, id, "approved", agent.ID); err != nil {
		return "", fmt.Errorf("update status: %w", err)
	}

	h.tokensMu.Lock()
	h.tokens[id] = approvedToken{raw: rawToken, approvedAt: time.Now()}
	h.tokensMu.Unlock()

	go h.cleanExpiredTokens()

	h.eventHub.Publish(userID, events.Event{Type: "queue"})

	return agent.ID, nil
}

// Deny handles POST /api/agents/connect/{id}/deny (user JWT).
func (h *ConnectionsHandler) Deny(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	if err := h.DenyByID(r.Context(), id, user.ID); err != nil {
		switch err {
		case store.ErrNotFound:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "connection request not found")
		case errForbidden:
			writeError(w, http.StatusForbidden, "FORBIDDEN", "not your connection request")
		case errAlreadyResolved:
			writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "connection request is not pending")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not deny connection request")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "denied"})
}

// DenyByID is the core deny logic, callable from HTTP handlers and
// the notifier decision consumer.
func (h *ConnectionsHandler) DenyByID(ctx context.Context, id, userID string) error {
	cr, err := h.st.GetConnectionRequest(ctx, id)
	if err != nil {
		return err
	}
	if cr.UserID != userID {
		return errForbidden
	}
	if cr.Status != "pending" {
		return errAlreadyResolved
	}

	if err := h.st.UpdateConnectionRequestStatus(ctx, id, "denied", ""); err != nil {
		return fmt.Errorf("update status: %w", err)
	}

	h.eventHub.Publish(userID, events.Event{Type: "queue"})
	return nil
}

// List handles GET /api/agents/connections (user JWT).
func (h *ConnectionsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	requests, err := h.st.ListPendingConnectionRequests(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list connection requests")
		return
	}
	writeJSON(w, http.StatusOK, requests)
}

// cleanExpiredTokens removes tokens older than the token window.
func (h *ConnectionsHandler) cleanExpiredTokens() {
	h.tokensMu.Lock()
	defer h.tokensMu.Unlock()
	now := time.Now()
	for id, tok := range h.tokens {
		if now.Sub(tok.approvedAt) > connectionTokenWindow {
			delete(h.tokens, id)
		}
	}
}
