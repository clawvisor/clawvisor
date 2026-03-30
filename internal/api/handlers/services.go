package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
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
	Alias        string   // "default" when not specified
	PendingReqID string   // pending_request_id query param (may be empty)
	CLICallback  string   // TUI local server callback URL (may be empty)
	Scopes       []string // merged scopes for this OAuth flow
	ExpiresAt    time.Time
}

// validAliasRe matches safe alias values: alphanumeric, underscores, hyphens.
var validAliasRe = regexp.MustCompile(`^[a-zA-Z0-9_-]*$`)

// validAlias returns true if s is a safe service alias (empty is OK, maps to "default").
func validAlias(s string) bool {
	return validAliasRe.MatchString(s)
}

// validateCLICallback checks that a CLI callback URL is safe — it must be
// http-only and point to localhost or 127.0.0.1. Returns "" if invalid.
func validateCLICallback(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" {
		return ""
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") {
		return ""
	}
	return raw
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
	// metaByKey maps "serviceID:alias" → meta.
	metaByKey := make(map[string]*store.ServiceMeta, len(metas))
	for _, m := range metas {
		metaByKey[m.ServiceID+":"+m.Alias] = m
	}

	type actionEntry struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Category    string `json:"category,omitempty"`
		Sensitivity string `json:"sensitivity,omitempty"`
	}

	type serviceEntry struct {
		ID                 string        `json:"id"`
		Name               string        `json:"name"`
		Description        string        `json:"description"`
		Alias              string        `json:"alias,omitempty"`
		OAuth              bool          `json:"oauth"`
		OAuthEndpoint      string        `json:"oauth_endpoint,omitempty"`
		RequiresActivation bool          `json:"requires_activation"`
		CredentialFree     bool          `json:"credential_free"`
		Actions            []actionEntry `json:"actions"`
		Status             string        `json:"status"`
		ActivatedAt        *time.Time    `json:"activated_at,omitempty"`
		SetupURL           string        `json:"setup_url,omitempty"`
	}

	// buildEntry creates a serviceEntry from an adapter, using MetadataProvider when available.
	buildEntry := func(a adapters.Adapter) serviceEntry {
		name := display.ServiceName(a.ServiceID())
		desc := display.ServiceDescription(a.ServiceID())
		var setupURL, oauthEndpoint string
		actionNames := map[string]adapters.ActionMeta{}

		if mp, ok := a.(adapters.MetadataProvider); ok {
			meta := mp.ServiceMetadata()
			if meta.DisplayName != "" {
				name = meta.DisplayName
			}
			if meta.Description != "" {
				desc = meta.Description
			}
			setupURL = meta.SetupURL
			oauthEndpoint = meta.OAuthEndpoint
			actionNames = meta.ActionMeta
		}

		actions := make([]actionEntry, 0, len(a.SupportedActions()))
		for _, actionID := range a.SupportedActions() {
			ae := actionEntry{ID: actionID, DisplayName: display.ActionName(actionID)}
			if am, ok := actionNames[actionID]; ok {
				if am.DisplayName != "" {
					ae.DisplayName = am.DisplayName
				}
				ae.Category = am.Category
				ae.Sensitivity = am.Sensitivity
			}
			actions = append(actions, ae)
		}

		return serviceEntry{
			ID:                 a.ServiceID(),
			Name:               name,
			Description:        desc,
			OAuth:              len(a.RequiredScopes()) > 0,
			OAuthEndpoint:      oauthEndpoint,
			RequiresActivation: true,
			Actions:            actions,
			SetupURL:           setupURL,
		}
	}

	services := make([]serviceEntry, 0)
	for _, a := range h.adapterReg.All() {
		credentialFree := a.ValidateCredential(nil) == nil

		if credentialFree {
			status := "not_activated"
			var activatedAt *time.Time
			if m, ok := metaByKey[a.ServiceID()+":default"]; ok {
				status = "activated"
				activatedAt = &m.ActivatedAt
			}
			entry := buildEntry(a)
			entry.CredentialFree = true
			entry.Status = status
			entry.ActivatedAt = activatedAt
			services = append(services, entry)
			continue
		}

		shown := false
		for _, m := range metas {
			if m.ServiceID != a.ServiceID() {
				continue
			}
			vKey := h.adapterReg.VaultKeyWithAlias(a.ServiceID(), m.Alias)
			if !keySet[vKey] {
				continue
			}
			shown = true
			alias := ""
			if m.Alias != "default" {
				alias = m.Alias
			}
			activatedAt := m.ActivatedAt
			entry := buildEntry(a)
			entry.Alias = alias
			entry.Status = "activated"
			entry.ActivatedAt = &activatedAt
			services = append(services, entry)
		}

		baseKey := h.adapterReg.VaultKey(a.ServiceID())
		usesSharedKey := baseKey != a.ServiceID()
		if !shown && !usesSharedKey && keySet[baseKey] {
			var activatedAt *time.Time
			if m, ok := metaByKey[a.ServiceID()+":default"]; ok {
				activatedAt = &m.ActivatedAt
			}
			entry := buildEntry(a)
			entry.Status = "activated"
			entry.ActivatedAt = activatedAt
			services = append(services, entry)
			shown = true
		}

		if !shown {
			entry := buildEntry(a)
			entry.Status = "not_activated"
			services = append(services, entry)
		}
	}
	sort.Slice(services, func(i, j int) bool {
		if services[i].ID != services[j].ID {
			return services[i].ID < services[j].ID
		}
		return services[i].Alias < services[j].Alias
	})
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

// OAuthGetURL returns the OAuth2 authorization URL as JSON without redirecting.
// The client fetches this endpoint (with Authorization header) and then navigates
// to the returned URL — e.g. window.open(url, '_blank').
//
// If the user already has credentials with all required scopes for this service,
// the response is {"already_authorized": true, "service": "..."} and no OAuth
// flow is needed.
//
// GET /api/oauth/url?service=google.gmail[&pending_request_id=...]
// Auth: user JWT
// Response: {"url": "https://accounts.google.com/..."} or {"already_authorized": true, ...}
func (h *ServicesHandler) OAuthGetURL(w http.ResponseWriter, r *http.Request) {
	h.sweepExpiredOAuthStates()

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

	alias := r.URL.Query().Get("alias")
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -)")
		return
	}

	mergedScopes, alreadyAuthorized := h.resolveOAuthScopes(r.Context(), user.ID, serviceID, alias, adapter)
	if alreadyAuthorized {
		// Scopes already granted — just ensure service_meta exists and return.
		_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, alias, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{
			"already_authorized": true,
			"service":            serviceID,
		})
		return
	}

	stateToken := uuid.New().String()
	h.oauthStates.Store(stateToken, oauthStateEntry{
		UserID:       user.ID,
		ServiceID:    serviceID,
		Alias:        alias,
		PendingReqID: r.URL.Query().Get("pending_request_id"),
		CLICallback:  validateCLICallback(r.URL.Query().Get("cli_callback")),
		Scopes:       mergedScopes,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	})

	oauthCfg.Scopes = mergedScopes
	authURL := oauthAuthURL(oauthCfg, stateToken, alias)
	writeJSON(w, http.StatusOK, map[string]string{"url": authURL})
}

// OAuthStart generates an OAuth2 consent URL and redirects the user.
//
// GET /api/oauth/start?service=google.gmail[&alias=personal][&pending_request_id=...]
// Auth: user JWT
func (h *ServicesHandler) OAuthStart(w http.ResponseWriter, r *http.Request) {
	h.sweepExpiredOAuthStates()

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

	alias := r.URL.Query().Get("alias")
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -)")
		return
	}

	mergedScopes, _ := h.resolveOAuthScopes(r.Context(), user.ID, serviceID, alias, adapter)

	stateToken := uuid.New().String()
	h.oauthStates.Store(stateToken, oauthStateEntry{
		UserID:       user.ID,
		ServiceID:    serviceID,
		Alias:        alias,
		PendingReqID: r.URL.Query().Get("pending_request_id"),
		Scopes:       mergedScopes,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	})

	oauthCfg.Scopes = mergedScopes
	authURL := oauthAuthURL(oauthCfg, stateToken, alias)
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
		oauthPopupClose(w, "Missing OAuth parameters.", "")
		return
	}

	val, ok := h.oauthStates.LoadAndDelete(state)
	if !ok {
		oauthPopupClose(w, "Invalid or expired OAuth state. Please try again.", "")
		return
	}
	entry := val.(oauthStateEntry)
	if time.Now().After(entry.ExpiresAt) {
		oauthPopupClose(w, "OAuth session expired. Please try again.", "")
		return
	}

	adapter, ok := h.adapterReg.Get(entry.ServiceID)
	if !ok {
		oauthPopupClose(w, "Service not found.", "")
		return
	}

	oauthCfg := adapter.OAuthConfig()
	// Use the merged scopes stored during URL generation.
	if len(entry.Scopes) > 0 {
		oauthCfg.Scopes = entry.Scopes
	}
	token, err := oauthCfg.Exchange(r.Context(), code)
	if err != nil {
		h.logger.Warn("oauth token exchange failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Token exchange with provider failed.", "")
		return
	}

	// Use the merged scopes from the state entry for the credential.
	scopes := entry.Scopes
	if len(scopes) == 0 {
		scopes = adapter.RequiredScopes()
	}

	// Preserve existing refresh token if Google didn't issue a new one on re-consent.
	alias := entry.Alias
	if alias == "" {
		alias = "default"
	}
	if token.RefreshToken == "" {
		if existing := h.loadExistingRefreshToken(r.Context(), entry.UserID, entry.ServiceID, alias); existing != "" {
			token.RefreshToken = existing
		}
	}

	credBytes, err := credential.FromToken(token, scopes)
	if err != nil {
		h.logger.Warn("credential from token failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Failed to process credential.", "")
		return
	}

	vKey := h.adapterReg.VaultKeyWithAlias(entry.ServiceID, alias)
	if err := h.vault.Set(r.Context(), entry.UserID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Failed to store credential in vault.", "")
		return
	}

	_ = h.st.UpsertServiceMeta(r.Context(), entry.UserID, entry.ServiceID, alias, time.Now())
	h.logger.Info("service activated", "user", entry.UserID, "service", entry.ServiceID)

	// Re-execute any pending request that was waiting for this activation.
	if entry.PendingReqID != "" {
		go h.reactivatePendingRequest(context.Background(), entry.UserID, entry.PendingReqID)
	}

	oauthPopupClose(w, "", entry.CLICallback)
}

// oauthPopupClose serves a minimal HTML page that closes the OAuth popup window.
// On success (errMsg == "") it posts a message to the opener so the dashboard can
// refresh its services list. On error it shows the message and auto-closes after 5s.
// If cliCallback is set, the success page also pings that URL to notify the TUI.
func oauthPopupClose(w http.ResponseWriter, errMsg, cliCallback string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Override the global CSP set by security middleware. This page uses
	// inline scripts (postMessage, window.close, fetch to TUI callback)
	// which are blocked by the default "script-src 'self'" policy.
	// connect-src http://localhost:* allows the fetch to the TUI's local
	// one-shot server when cli_callback is provided.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src http://localhost:* http://127.0.0.1:*")
	if errMsg != "" {
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Error – Clawvisor</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f9fafb}
.card{background:#fff;border-radius:8px;padding:32px;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.1);max-width:320px}
h2{color:#dc2626;margin:0 0 8px}p{color:#6b7280;margin:4px 0;font-size:14px}</style></head>
<body><div class="card"><h2>Authorization failed</h2><p>%s</p><p>You can close this tab.</p></div>
</body></html>`,
			html.EscapeString(errMsg))
		return
	}
	// Build the CLI callback fetch snippet if a callback URL was provided.
	cliCallbackSnippet := ""
	if cliCallback != "" {
		cliCallbackSnippet = fmt.Sprintf("fetch(%q).catch(function(){});", cliCallback)
	}
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Authorized – Clawvisor</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f9fafb}
.card{background:#fff;border-radius:8px;padding:32px;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.1);max-width:320px}
h2{color:#16a34a;margin:0 0 8px}p{color:#6b7280;margin:0;font-size:14px}</style></head>
<body><div class="card"><h2>&#10003; Authorized</h2><p>Service activated. You can close this tab.</p></div>
<script>
if(window.opener){try{window.opener.postMessage({type:'clawvisor_oauth_done'},'*')}catch(e){}}
%stry{window.close()}catch(e){}
</script></body></html>`, cliCallbackSnippet)
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
			Alias            string `json:"alias"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
		alias := body.Alias
		if alias == "" {
			alias = "default"
		}
		if !validAlias(alias) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -)")
			return
		}

		mergedScopes, alreadyAuthorized := h.resolveOAuthScopes(r.Context(), user.ID, serviceID, alias, adapter)
		if alreadyAuthorized {
			_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, alias, time.Now())
			writeJSON(w, http.StatusOK, map[string]any{
				"already_authorized": true,
				"service":            serviceID,
			})
			return
		}

		stateToken := uuid.New().String()
		h.oauthStates.Store(stateToken, oauthStateEntry{
			UserID:       user.ID,
			ServiceID:    serviceID,
			Alias:        alias,
			PendingReqID: body.PendingRequestID,
			Scopes:       mergedScopes,
			ExpiresAt:    time.Now().Add(10 * time.Minute),
		})
		oauthCfg := adapter.OAuthConfig()
		oauthCfg.Scopes = mergedScopes
		authURL := oauthAuthURL(oauthCfg, stateToken, alias)
		writeJSON(w, http.StatusOK, map[string]string{"url": authURL})
		return
	}

	// Credential-free service (e.g. iMessage): create service_meta record, no vault op.
	if adapter.ValidateCredential(nil) == nil {
		_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, "default", time.Now())
		h.logger.Info("credential-free service activated", "user", user.ID, "service", serviceID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "activated", "service": serviceID})
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
		Alias string `json:"alias"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "token is required")
		return
	}
	alias := body.Alias
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -)")
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

	vKey := h.adapterReg.VaultKeyWithAlias(serviceID, alias)
	if err := h.vault.Set(r.Context(), user.ID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed (api key)", "service", serviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "VAULT_ERROR", "failed to store credential")
		return
	}
	_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, alias, time.Now())
	h.logger.Info("service activated via api key", "user", user.ID, "service", serviceID, "alias", alias)

	writeJSON(w, http.StatusOK, map[string]string{"status": "activated", "service": serviceID})
}

// Deactivate removes the credential and service_meta for a service + alias.
//
// POST /api/services/{serviceID}/deactivate
// Auth: user JWT
// Body: {"alias": "..."} (optional; defaults to "default")
func (h *ServicesHandler) Deactivate(w http.ResponseWriter, r *http.Request) {
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

	var body struct {
		Alias string `json:"alias"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	alias := body.Alias
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -)")
		return
	}

	// Remove the service_meta record first.
	_ = h.st.DeleteServiceMeta(r.Context(), user.ID, serviceID, alias)

	// Credential-free services have no vault credential to clean up.
	adapter, ok := h.adapterReg.Get(serviceID)
	if ok && adapter.ValidateCredential(nil) == nil {
		h.logger.Info("credential-free service deactivated", "user", user.ID, "service", serviceID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated", "service": serviceID})
		return
	}

	// Google services share the vault key "google" (or "google:<alias>").
	// If other services still reference the same vault key, strip the
	// deactivated service's scopes from the stored credential instead of
	// deleting it. This ensures resolveOAuthScopes will re-request consent
	// if the service is re-activated later.
	vKey := h.adapterReg.VaultKeyWithAlias(serviceID, alias)
	metas, _ := h.st.ListServiceMetas(r.Context(), user.ID)
	otherUsesKey := false
	for _, m := range metas {
		if h.adapterReg.VaultKeyWithAlias(m.ServiceID, m.Alias) == vKey {
			otherUsesKey = true
			break
		}
	}
	if otherUsesKey {
		// Strip the deactivated service's scopes from the shared credential.
		adapter, ok := h.adapterReg.Get(serviceID)
		if ok {
			h.removeAdapterScopes(r.Context(), user.ID, vKey, adapter)
		}
	} else {
		_ = h.vault.Delete(r.Context(), user.ID, vKey)
	}

	h.logger.Info("service deactivated", "user", user.ID, "service", serviceID, "alias", alias)

	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated", "service": serviceID})
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

	serviceType, alias := parseServiceAlias(blob.Service)
	vKey := h.adapterReg.VaultKeyWithAlias(serviceType, alias)
	result, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg,
		userID, blob.Service, blob.Action, blob.Params, vKey)

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
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, blob.AgentID)
		_ = callback.DeliverResult(ctx, *pa.CallbackURL, &callback.Payload{
			Type:      "request",
			RequestID: requestID,
			Status:    outcome,
			Result:    cbResult,
			AuditID:   pa.AuditID,
		}, cbKey)
	}

	h.logger.Info("pending request re-executed after activation",
		"request_id", requestID, "outcome", outcome)
}

// resolveOAuthScopes checks whether the user already has a credential with
// sufficient scopes for the requested service. If so, alreadyAuthorized is true.
// Otherwise, it returns the merged set of existing + required scopes.
func (h *ServicesHandler) resolveOAuthScopes(
	ctx context.Context,
	userID, serviceID, alias string,
	adapter adapters.Adapter,
) (mergedScopes []string, alreadyAuthorized bool) {
	requiredScopes := adapter.RequiredScopes()
	if len(requiredScopes) == 0 {
		// Non-Google adapter or no scopes declared — use the adapter's default.
		return adapter.OAuthConfig().Scopes, false
	}

	// Check for existing credential in the vault.
	vKey := h.adapterReg.VaultKeyWithAlias(serviceID, alias)
	existingBytes, err := h.vault.Get(ctx, userID, vKey)
	if err != nil || len(existingBytes) == 0 {
		// No existing credential — just use this adapter's scopes.
		return requiredScopes, false
	}

	existingCred, err := credential.Parse(existingBytes)
	if err != nil {
		// Invalid credential — treat as no credential.
		return requiredScopes, false
	}

	// If existing credential already has all required scopes, no OAuth needed.
	if credential.HasAllScopes(existingCred.Scopes, requiredScopes) {
		return existingCred.Scopes, true
	}

	// Merge existing + new scopes for incremental consent.
	return credential.MergeScopes(existingCred.Scopes, requiredScopes), false
}

// sweepExpiredOAuthStates removes OAuth state entries older than 10 minutes.
// Called lazily on each new OAuth URL generation.
func (h *ServicesHandler) sweepExpiredOAuthStates() {
	now := time.Now()
	h.oauthStates.Range(func(key, value any) bool {
		entry := value.(oauthStateEntry)
		if now.After(entry.ExpiresAt) {
			h.oauthStates.Delete(key)
		}
		return true
	})
}

// oauthAuthURL builds the OAuth2 authorization URL. For non-default aliases
// (multi-account), it adds prompt=select_account so the user can choose a
// different Google account.
func oauthAuthURL(cfg *oauth2.Config, stateToken, alias string) string {
	opts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("include_granted_scopes", "true"),
		oauth2.SetAuthURLParam("prompt", "consent"),
	}
	if alias != "" && alias != "default" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "consent select_account"))
	}
	return cfg.AuthCodeURL(stateToken, opts...)
}

// loadExistingRefreshToken retrieves the refresh token from an existing vault
// credential, if any. Google may not re-issue a refresh token on re-consent.
func (h *ServicesHandler) loadExistingRefreshToken(ctx context.Context, userID, serviceID, alias string) string {
	vKey := h.adapterReg.VaultKeyWithAlias(serviceID, alias)
	existingBytes, err := h.vault.Get(ctx, userID, vKey)
	if err != nil || len(existingBytes) == 0 {
		return ""
	}
	cred, err := credential.Parse(existingBytes)
	if err != nil {
		return ""
	}
	return cred.RefreshToken
}

// removeAdapterScopes strips the adapter's RequiredScopes from the stored
// vault credential. Called during deactivation when other services still
// share the same vault key — prevents resolveOAuthScopes from returning
// already_authorized for a service the user explicitly deactivated.
func (h *ServicesHandler) removeAdapterScopes(ctx context.Context, userID, vKey string, adapter adapters.Adapter) {
	scopes := adapter.RequiredScopes()
	if len(scopes) == 0 {
		return
	}
	existingBytes, err := h.vault.Get(ctx, userID, vKey)
	if err != nil || len(existingBytes) == 0 {
		return
	}
	cred, err := credential.Parse(existingBytes)
	if err != nil {
		return
	}

	remove := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		remove[s] = true
	}
	filtered := make([]string, 0, len(cred.Scopes))
	for _, s := range cred.Scopes {
		if !remove[s] {
			filtered = append(filtered, s)
		}
	}
	cred.Scopes = filtered

	updated, err := json.Marshal(cred)
	if err != nil {
		return
	}
	_ = h.vault.Set(ctx, userID, vKey, updated)
}

// ── System OAuth Config ──────────────────────────────────────────────────────

// GetGoogleOAuthConfig checks whether Google OAuth app credentials are configured.
//
// GET /api/system/google-oauth
// Auth: user JWT
// Response: {"configured": true} or {"configured": false}
func (h *ServicesHandler) GetGoogleOAuthConfig(w http.ResponseWriter, r *http.Request) {
	clientID, _ := adapters.GetGoogleOAuthCredentials(r.Context(), h.vault)
	writeJSON(w, http.StatusOK, map[string]any{"configured": clientID != ""})
}

// SetGoogleOAuthConfig stores Google OAuth app credentials in the system vault.
// Once stored, Google adapters will immediately start returning OAuth configs
// (no restart required).
//
// POST /api/system/google-oauth
// Auth: user JWT
// Body: {"client_id": "...", "client_secret": "..."}
// Response: {"ok": true}
func (h *ServicesHandler) SetGoogleOAuthConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return
	}
	if body.ClientID == "" || body.ClientSecret == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "client_id and client_secret are required")
		return
	}

	if err := adapters.SetGoogleOAuthCredentials(r.Context(), h.vault, body.ClientID, body.ClientSecret); err != nil {
		h.logger.Error("failed to store Google OAuth credentials", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to store credentials")
		return
	}

	h.logger.Info("Google OAuth credentials stored in system vault")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
