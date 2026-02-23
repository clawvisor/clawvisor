package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/api/middleware"
	"github.com/ericlevine/clawvisor/internal/callback"
	"github.com/ericlevine/clawvisor/internal/store"
	"github.com/ericlevine/clawvisor/internal/vault"
)

// ServicesHandler serves the service catalog and OAuth activation flow.
type ServicesHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	logger     *slog.Logger
	baseURL    string

	// oauthStates holds temporary OAuth2 state tokens (in-memory; Phase 3 only).
	oauthStates sync.Map // stateToken (string) → oauthStateEntry
}

type oauthStateEntry struct {
	UserID       string
	ServiceID    string
	PendingReqID string // pending_request_id query param (may be empty)
	ExpiresAt    time.Time
}

func NewServicesHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, logger *slog.Logger, baseURL string) *ServicesHandler {
	return &ServicesHandler{
		st: st, vault: v, adapterReg: adapterReg, logger: logger, baseURL: baseURL,
	}
}

// List returns the service catalog with per-user activation status.
//
// GET /api/services
// Auth: user JWT
func (h *ServicesHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	// Which vault keys are activated for this user.
	activatedKeys, _ := h.vault.List(r.Context(), user.ID)
	keySet := make(map[string]bool, len(activatedKeys))
	for _, k := range activatedKeys {
		keySet[k] = true
	}

	// Service meta records (for activated_at timestamps).
	metas, _ := h.st.ListServiceMetas(r.Context(), user.ID)
	metaMap := make(map[string]*store.ServiceMeta, len(metas))
	for _, m := range metas {
		metaMap[m.ServiceID] = m
	}

	type serviceEntry struct {
		ID          string     `json:"id"`
		OAuth       bool       `json:"oauth"`
		Actions     []string   `json:"actions"`
		Status      string     `json:"status"`
		ActivatedAt *time.Time `json:"activated_at,omitempty"`
	}

	services := make([]serviceEntry, 0)
	for _, a := range h.adapterReg.All() {
		vKey := vaultKeyForService(a.ServiceID())
		status := "not_activated"
		var activatedAt *time.Time
		if keySet[vKey] {
			status = "activated"
			if m, ok := metaMap[a.ServiceID()]; ok {
				activatedAt = &m.ActivatedAt
			}
		}
		services = append(services, serviceEntry{
			ID:          a.ServiceID(),
			OAuth:       a.OAuthConfig() != nil,
			Actions:     a.SupportedActions(),
			Status:      status,
			ActivatedAt: activatedAt,
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].ID < services[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

// OAuthStart generates an OAuth2 consent URL and redirects the user.
//
// GET /api/oauth/start?service=google.gmail[&pending_request_id=...]
// Auth: user JWT
func (h *ServicesHandler) OAuthStart(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	serviceID := r.URL.Query().Get("service")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service is required")
		return
	}

	adapter, ok := h.adapterReg.Get(serviceID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("service %q not found", serviceID))
		return
	}

	oauthCfg := adapter.OAuthConfig()
	if oauthCfg == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service does not use OAuth2")
		return
	}

	stateToken := uuid.New().String()
	h.oauthStates.Store(stateToken, oauthStateEntry{
		UserID:       user.ID,
		ServiceID:    serviceID,
		PendingReqID: r.URL.Query().Get("pending_request_id"),
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	})

	authURL := oauthCfg.AuthCodeURL(stateToken)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// OAuthCallback exchanges the authorization code for tokens and stores the credential.
//
// GET /api/oauth/callback?code=...&state=...
func (h *ServicesHandler) OAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	if state == "" || code == "" {
		http.Redirect(w, r, h.baseURL+"/?error=oauth_missing_params", http.StatusFound)
		return
	}

	val, ok := h.oauthStates.LoadAndDelete(state)
	if !ok {
		http.Redirect(w, r, h.baseURL+"/?error=oauth_invalid_state", http.StatusFound)
		return
	}
	entry := val.(oauthStateEntry)
	if time.Now().After(entry.ExpiresAt) {
		http.Redirect(w, r, h.baseURL+"/?error=oauth_state_expired", http.StatusFound)
		return
	}

	adapter, ok := h.adapterReg.Get(entry.ServiceID)
	if !ok {
		http.Redirect(w, r, h.baseURL+"/?error=service_not_found", http.StatusFound)
		return
	}

	oauthCfg := adapter.OAuthConfig()
	token, err := oauthCfg.Exchange(r.Context(), code)
	if err != nil {
		h.logger.Warn("oauth token exchange failed", "service", entry.ServiceID, "err", err)
		http.Redirect(w, r, h.baseURL+"/?error=oauth_exchange_failed", http.StatusFound)
		return
	}

	credBytes, err := adapter.CredentialFromToken(token)
	if err != nil {
		h.logger.Warn("credential from token failed", "service", entry.ServiceID, "err", err)
		http.Redirect(w, r, h.baseURL+"/?error=credential_failed", http.StatusFound)
		return
	}

	vKey := vaultKeyForService(entry.ServiceID)
	if err := h.vault.Set(r.Context(), entry.UserID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed", "service", entry.ServiceID, "err", err)
		http.Redirect(w, r, h.baseURL+"/?error=vault_failed", http.StatusFound)
		return
	}

	_ = h.st.UpsertServiceMeta(r.Context(), entry.UserID, entry.ServiceID, time.Now())
	h.logger.Info("service activated", "user", entry.UserID, "service", entry.ServiceID)

	// Re-execute any pending request that was waiting for this activation.
	if entry.PendingReqID != "" {
		go h.reactivatePendingRequest(context.Background(), entry.UserID, entry.PendingReqID)
	}

	http.Redirect(w, r, h.baseURL+"/", http.StatusFound)
}

// reactivatePendingRequest re-executes a pending request after service activation.
func (h *ServicesHandler) reactivatePendingRequest(ctx context.Context, userID, requestID string) {
	pa, err := h.st.GetPendingApproval(ctx, requestID)
	if err != nil {
		h.logger.Warn("reactivate: pending approval not found", "request_id", requestID, "err", err)
		return
	}

	var blob pendingRequestBlob
	if err := json.Unmarshal(pa.RequestBlob, &blob); err != nil {
		h.logger.Warn("reactivate: invalid request blob", "request_id", requestID, "err", err)
		return
	}

	result, _, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg,
		userID, blob.Service, blob.Action, blob.Params, blob.ResponseFilters)

	outcome := "executed"
	errMsg := ""
	if execErr != nil {
		outcome = "error"
		errMsg = execErr.Error()
	}

	_ = h.st.UpdateAuditOutcome(ctx, pa.AuditID, outcome, errMsg, 0)
	_ = h.st.DeletePendingApproval(ctx, requestID)

	if pa.CallbackURL != nil && *pa.CallbackURL != "" {
		var cbResult *adapters.Result
		if execErr == nil {
			cbResult = result
		}
		_ = callback.DeliverResult(ctx, *pa.CallbackURL, &callback.Payload{
			RequestID: requestID,
			Status:    outcome,
			Result:    cbResult,
			AuditID:   pa.AuditID,
		})
	}

	h.logger.Info("pending request re-executed after activation",
		"request_id", requestID, "outcome", outcome)
}
