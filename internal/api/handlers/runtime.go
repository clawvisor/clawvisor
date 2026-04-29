package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type RuntimeManager interface {
	CreateRuntimeSession(ctx context.Context, agentID, userID string, req runtimeproxy.CreateSessionRequest) (*runtimeproxy.CreateSessionResult, error)
	ListRuntimeSessionsForUser(ctx context.Context, userID string) ([]*store.RuntimeSession, error)
	RevokeRuntimeSession(ctx context.Context, sessionID string) error
	ProxyURL() string
	CACertPEM() string
}

type RuntimeHandler struct {
	st      store.Store
	manager RuntimeManager
	cfg     *config.Config
}

func NewRuntimeHandler(st store.Store, manager RuntimeManager, cfg *config.Config) *RuntimeHandler {
	return &RuntimeHandler{st: st, manager: manager, cfg: cfg}
}

func (h *RuntimeHandler) CreateSession(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.manager == nil || h.cfg == nil || !h.cfg.RuntimeProxy.Enabled {
		writeError(w, http.StatusConflict, "RUNTIME_PROXY_DISABLED", "runtime proxy is not enabled")
		return
	}
	var req struct {
		Mode            string         `json:"mode"`
		ObservationMode *bool          `json:"observation_mode,omitempty"`
		TTLSeconds      int            `json:"ttl_seconds,omitempty"`
		Metadata        map[string]any `json:"metadata,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.manager.CreateRuntimeSession(r.Context(), agent.ID, agent.UserID, runtimeproxy.CreateSessionRequest{
		Mode:            req.Mode,
		ObservationMode: req.ObservationMode,
		TTLSeconds:      req.TTLSeconds,
		Metadata:        req.Metadata,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create runtime session")
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *RuntimeHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	sessions, err := h.manager.ListRuntimeSessionsForUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": sessions,
		"total":   len(sessions),
	})
}

func (h *RuntimeHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	sessionID := r.PathValue("id")
	session, err := h.st.GetRuntimeSession(r.Context(), sessionID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime session")
		return
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime session")
		return
	}
	if err := h.manager.RevokeRuntimeSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke runtime session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"status":     "revoked",
	})
}

func (h *RuntimeHandler) Status(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	_ = user
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":                  h.cfg != nil && h.cfg.RuntimeProxy.Enabled,
		"proxy_url":                h.manager.ProxyURL(),
		"observation_mode_default": h.cfg != nil && h.cfg.RuntimePolicy.ObservationModeDefault,
		"inline_approval_enabled":  h.cfg != nil && h.cfg.RuntimePolicy.InlineApprovalEnabled,
		"tool_lease_timeout_seconds": func() int {
			if h.cfg == nil {
				return 0
			}
			return h.cfg.RuntimePolicy.ToolLeaseTimeoutSeconds
		}(),
		"one_off_ttl_seconds": func() int {
			if h.cfg == nil {
				return 0
			}
			return h.cfg.RuntimePolicy.OneOffTTLSeconds
		}(),
		"ca_cert_pem": h.manager.CACertPEM(),
	})
}

func (h *RuntimeHandler) ListApprovals(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	records, err := h.st.ListPendingApprovalRecords(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime approvals")
		return
	}
	var filtered []*store.ApprovalRecord
	for _, rec := range records {
		if rec.ResolutionTransport == "consume_one_off_retry" || rec.ResolutionTransport == "release_held_tool_use" {
			filtered = append(filtered, rec)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": filtered,
		"total":   len(filtered),
	})
}

func (h *RuntimeHandler) ResolveApproval(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	approvalID := r.PathValue("id")
	rec, err := h.st.GetApprovalRecord(r.Context(), approvalID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime approval")
		return
	}
	if rec.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime approval")
		return
	}
	var req struct {
		Resolution string `json:"resolution"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Resolution != "allow_once" && req.Resolution != "deny" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "resolution must be allow_once or deny")
		return
	}
	if req.Resolution == "allow_once" && rec.ResolutionTransport == "consume_one_off_retry" {
		var payload runtimeproxy.RuntimeApprovalPayload
		if err := json.Unmarshal(rec.PayloadJSON, &payload); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not parse runtime approval payload")
			return
		}
		if err := h.st.CreateOneOffApproval(r.Context(), &store.OneOffApproval{
			SessionID:          payload.SessionID,
			RequestFingerprint: payload.RequestFingerprint,
			ApprovalID:         &rec.ID,
			ApprovedAt:         time.Now().UTC(),
			ExpiresAt:          time.Now().UTC().Add(time.Duration(h.cfg.RuntimePolicy.OneOffTTLSeconds) * time.Second),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create one-off approval")
			return
		}
	}
	status := "approved"
	if req.Resolution == "deny" {
		status = "denied"
	}
	if err := h.st.ResolveApprovalRecord(r.Context(), rec.ID, req.Resolution, status, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not resolve runtime approval")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approval_id": rec.ID,
		"status":      status,
		"resolution":  req.Resolution,
	})
}

func (h *RuntimeHandler) ListLeases(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "session_id is required")
		return
	}
	session, err := h.st.GetRuntimeSession(r.Context(), sessionID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime session")
		return
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime session")
		return
	}
	leases, err := h.st.ListOpenToolExecutionLeases(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime leases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": leases,
		"total":   len(leases),
	})
}
