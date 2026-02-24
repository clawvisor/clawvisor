package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
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
		ID                 string     `json:"id"`
		OAuth              bool       `json:"oauth"`
		RequiresActivation bool       `json:"requires_activation"`
		Actions            []string   `json:"actions"`
		Status             string     `json:"status"`
		ActivatedAt        *time.Time `json:"activated_at,omitempty"`
	}

	services := make([]serviceEntry, 0)
	for _, a := range h.adapterReg.All() {
		credentialFree := a.ValidateCredential(nil) == nil
		vKey := vaultKeyForService(a.ServiceID())
		status := "not_activated"
		var activatedAt *time.Time
		if credentialFree || keySet[vKey] {
			status = "activated"
			if m, ok := metaMap[a.ServiceID()]; ok {
				activatedAt = &m.ActivatedAt
			}
		}
		services = append(services, serviceEntry{
			ID:                 a.ServiceID(),
			OAuth:              a.OAuthConfig() != nil,
			RequiresActivation: !credentialFree,
			Actions:            a.SupportedActions(),
			Status:             status,
			ActivatedAt:        activatedAt,
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].ID < services[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

// OAuthGetURL returns the OAuth2 authorization URL as JSON without redirecting.
// The client fetches this endpoint (with Authorization header) and then navigates
// to the returned URL — e.g. window.open(url, '_blank').
//
// GET /api/oauth/url?service=google.gmail[&pending_request_id=...]
// Auth: user JWT
// Response: {"url": "https://accounts.google.com/..."}
func (h *ServicesHandler) OAuthGetURL(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]string{"url": authURL})
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
// It serves an HTML page that closes the popup and notifies the opener via postMessage,
// rather than redirecting — the dashboard stays open throughout the OAuth flow.
//
// GET /api/oauth/callback?code=...&state=...
func (h *ServicesHandler) OAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	if state == "" || code == "" {
		oauthPopupClose(w, "Missing OAuth parameters.")
		return
	}

	val, ok := h.oauthStates.LoadAndDelete(state)
	if !ok {
		oauthPopupClose(w, "Invalid or expired OAuth state. Please try again.")
		return
	}
	entry := val.(oauthStateEntry)
	if time.Now().After(entry.ExpiresAt) {
		oauthPopupClose(w, "OAuth session expired. Please try again.")
		return
	}

	adapter, ok := h.adapterReg.Get(entry.ServiceID)
	if !ok {
		oauthPopupClose(w, "Service not found.")
		return
	}

	oauthCfg := adapter.OAuthConfig()
	token, err := oauthCfg.Exchange(r.Context(), code)
	if err != nil {
		h.logger.Warn("oauth token exchange failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Token exchange with provider failed.")
		return
	}

	credBytes, err := adapter.CredentialFromToken(token)
	if err != nil {
		h.logger.Warn("credential from token failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Failed to process credential.")
		return
	}

	vKey := vaultKeyForService(entry.ServiceID)
	if err := h.vault.Set(r.Context(), entry.UserID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Failed to store credential in vault.")
		return
	}

	_ = h.st.UpsertServiceMeta(r.Context(), entry.UserID, entry.ServiceID, time.Now())
	h.logger.Info("service activated", "user", entry.UserID, "service", entry.ServiceID)

	// Re-execute any pending request that was waiting for this activation.
	if entry.PendingReqID != "" {
		go h.reactivatePendingRequest(context.Background(), entry.UserID, entry.PendingReqID)
	}

	oauthPopupClose(w, "")
}

// oauthPopupClose serves a minimal HTML page that closes the OAuth popup window.
// On success (errMsg == "") it posts a message to the opener so the dashboard can
// refresh its services list. On error it shows the message and auto-closes after 5s.
func oauthPopupClose(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Error – Clawvisor</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f9fafb}
.card{background:#fff;border-radius:8px;padding:32px;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.1);max-width:320px}
h2{color:#dc2626;margin:0 0 8px}p{color:#6b7280;margin:4px 0;font-size:14px}</style></head>
<body><div class="card"><h2>Authorization failed</h2><p>%s</p><p>This window will close automatically.</p></div>
<script>setTimeout(function(){window.close()},5000)</script></body></html>`,
			html.EscapeString(errMsg))
		return
	}
	fmt.Fprint(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Authorized – Clawvisor</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f9fafb}
.card{background:#fff;border-radius:8px;padding:32px;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.1);max-width:320px}
h2{color:#16a34a;margin:0 0 8px}p{color:#6b7280;margin:0;font-size:14px}</style></head>
<body><div class="card"><h2>&#10003; Authorized</h2><p>Service activated. This window will close automatically.</p></div>
<script>
if(window.opener){try{window.opener.postMessage({type:'clawvisor_oauth_done'},'*')}catch(e){}}
setTimeout(function(){window.close()},1500)
</script></body></html>`)
}

// Activate is a unified activation endpoint.
// For OAuth services: returns the OAuth authorization URL as JSON (no redirect).
// For API key services: delegates to ActivateWithKey.
//
// POST /api/services/{serviceID}/activate
// Auth: user JWT
// OAuth body: {} or {"pending_request_id": "..."} — returns {"url": "https://..."}
// API key body: {"token": "ghp_..."} — returns {"status": "activated", "service": "..."}
func (h *ServicesHandler) Activate(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	serviceID := r.PathValue("serviceID")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "serviceID is required")
		return
	}

	adapter, ok := h.adapterReg.Get(serviceID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("service %q not found", serviceID))
		return
	}

	if adapter.OAuthConfig() != nil {
		// OAuth service: generate state token and return the consent URL as JSON.
		var body struct {
			PendingRequestID string `json:"pending_request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // body is optional

		stateToken := uuid.New().String()
		h.oauthStates.Store(stateToken, oauthStateEntry{
			UserID:       user.ID,
			ServiceID:    serviceID,
			PendingReqID: body.PendingRequestID,
			ExpiresAt:    time.Now().Add(10 * time.Minute),
		})
		authURL := adapter.OAuthConfig().AuthCodeURL(stateToken)
		writeJSON(w, http.StatusOK, map[string]string{"url": authURL})
		return
	}

	// API key service: delegate to the existing activate-key handler.
	h.ActivateWithKey(w, r)
}

// ActivateWithKey activates a non-OAuth service (e.g. GitHub) using an API key.
//
// POST /api/services/{serviceID}/activate-key
// Auth: user JWT
// Body: {"token": "ghp_..."}
func (h *ServicesHandler) ActivateWithKey(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	serviceID := r.PathValue("serviceID")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "serviceID is required")
		return
	}

	adapter, ok := h.adapterReg.Get(serviceID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("service %q not found", serviceID))
		return
	}
	if adapter.OAuthConfig() != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service uses OAuth — use /api/oauth/start instead")
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "token is required")
		return
	}

	// Build and validate the credential bytes.
	credBytes, err := json.Marshal(map[string]string{"type": "api_key", "token": body.Token})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to encode credential")
		return
	}
	if err := adapter.ValidateCredential(credBytes); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIAL", err.Error())
		return
	}

	vKey := vaultKeyForService(serviceID)
	if err := h.vault.Set(r.Context(), user.ID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed (api key)", "service", serviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "VAULT_ERROR", "failed to store credential")
		return
	}
	_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, time.Now())
	h.logger.Info("service activated via api key", "user", user.ID, "service", serviceID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "activated", "service": serviceID})
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
		userID, blob.Service, blob.Action, blob.Params, blob.ResponseFilters, nil)

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
		}, blob.CallbackKey)
	}

	h.logger.Info("pending request re-executed after activation",
		"request_id", requestID, "outcome", outcome)
}
