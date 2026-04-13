package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
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
	eventHub   events.EventHub

	// oauthStore holds temporary OAuth flow state (standard, device, PKCE).
	// Backed by either in-memory or Redis, depending on server configuration.
	oauthStore OAuthStateStore

	// relayDaemonURL is the public HTTPS URL via the relay (e.g. "https://relay.clawvisor.com/d/DAEMON_ID").
	// Used as redirect_uri for PKCE flows that require HTTPS. Empty when relay is not configured.
	relayDaemonURL string
}

type oauthStateEntry struct {
	UserID       string
	ServiceID    string
	Alias        string            // "default" when not specified
	PendingReqID string            // pending_request_id query param (may be empty)
	CLICallback  string            // TUI local server callback URL (may be empty)
	Scopes       []string          // merged scopes for this OAuth flow
	Config       map[string]string // per-service variable values (may be nil)
	TokenPath    string            // JSON path to access token in token response (e.g. "authed_user.access_token")
	ExpiresAt    time.Time
}

var errEmptyAccessToken = errors.New("oauth token response missing access token")

// validAliasRe matches safe alias values: alphanumeric, underscores, hyphens,
// dots, spaces, and @ (to support auto-detected identities like emails and workspace names).
var validAliasRe = regexp.MustCompile(`^[a-zA-Z0-9_.@+ -]*$`)

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

func NewServicesHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry, logger *slog.Logger, baseURL string, eventHub events.EventHub) *ServicesHandler {
	return &ServicesHandler{
		st: st, vault: v, adapterReg: adapterReg, logger: logger, baseURL: baseURL, eventHub: eventHub,
		oauthStore: newMemoryOAuthStateStore(),
	}
}

// SetOAuthStateStore overrides the default in-memory OAuth state store.
func (h *ServicesHandler) SetOAuthStateStore(s OAuthStateStore) {
	h.oauthStore = s
}

// SetRelayDaemonURL sets the public HTTPS relay URL used for PKCE flow redirects.
func (h *ServicesHandler) SetRelayDaemonURL(u string) { h.relayDaemonURL = u }

// oauthRedirectURL returns the OAuth callback URL derived from the server's base URL.
func (h *ServicesHandler) oauthRedirectURL() string {
	return strings.TrimRight(h.baseURL, "/") + "/api/oauth/callback"
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
		ID                   string                  `json:"id"`
		Name                 string                  `json:"name"`
		Description          string                  `json:"description"`
		IconSVG              string                  `json:"icon_svg,omitempty"`
		Alias                string                  `json:"alias,omitempty"`
		OAuth                bool                    `json:"oauth"`
		OAuthEndpoint        string                  `json:"oauth_endpoint,omitempty"`
		DeviceFlow           bool                    `json:"device_flow,omitempty"`
		PKCEFlow             bool                    `json:"pkce_flow,omitempty"`
		PKCEClientIDRequired bool                    `json:"pkce_client_id_required,omitempty"`
		AutoIdentity         bool                    `json:"auto_identity,omitempty"`
		RequiresActivation   bool                    `json:"requires_activation"`
		CredentialFree       bool                    `json:"credential_free"`
		Actions              []actionEntry           `json:"actions"`
		Variables            []adapters.VariableMeta `json:"variables,omitempty"`
		Status               string                  `json:"status"`
		ActivatedAt          *time.Time              `json:"activated_at,omitempty"`
		SetupURL             string                  `json:"setup_url,omitempty"`
		KeyHint              string                  `json:"key_hint,omitempty"`
	}

	// buildEntry creates a serviceEntry from an adapter, using MetadataProvider when available.
	buildEntry := func(a adapters.Adapter) serviceEntry {
		name := display.ServiceName(a.ServiceID())
		desc := display.ServiceDescription(a.ServiceID())
		var setupURL, oauthEndpoint, iconSVG, keyHint string
		var variables []adapters.VariableMeta
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
			iconSVG = meta.IconSVG
			oauthEndpoint = meta.OAuthEndpoint
			keyHint = meta.KeyHint
			actionNames = meta.ActionMeta
			variables = meta.Variables
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

		var deviceFlow, pkceFlow, pkceFlowDefined bool
		if mp, ok2 := a.(adapters.MetadataProvider); ok2 {
			meta := mp.ServiceMetadata()
			deviceFlow = meta.DeviceFlow
			pkceFlow = meta.PKCEFlow
			pkceFlowDefined = meta.PKCEFlowDefined
		}
		_, autoIdentity := a.(adapters.IdentityFetcher)

		// If PKCE is defined but no client ID is configured yet, check the vault.
		if pkceFlowDefined && !pkceFlow {
			if cid := adapters.GetPKCEClientID(r.Context(), h.vault, a.ServiceID()); cid != "" {
				pkceFlow = true
			}
		}

		return serviceEntry{
			ID:                   a.ServiceID(),
			Name:                 name,
			Description:          desc,
			IconSVG:              iconSVG,
			OAuth:                a.RequiredScopes() != nil,
			OAuthEndpoint:        oauthEndpoint,
			DeviceFlow:           deviceFlow,
			PKCEFlow:             pkceFlowDefined,              // show PKCE option if defined, even without client ID
			PKCEClientIDRequired: pkceFlowDefined && !pkceFlow, // client ID still needed
			AutoIdentity:         autoIdentity,
			RequiresActivation:   true,
			Actions:              actions,
			Variables:            variables,
			SetupURL:             setupURL,
			KeyHint:              keyHint,
		}
	}

	services := make([]serviceEntry, 0)
	for _, a := range h.adapterReg.All() {
		if ac, ok := a.(adapters.AvailabilityChecker); ok && !ac.Available() {
			continue
		}
		credentialFree := a.ValidateCredential(nil) == nil

		if credentialFree {
			// Check all metas for this service (may have a non-default alias via rename).
			found := false
			for _, m := range metas {
				if m.ServiceID != a.ServiceID() {
					continue
				}
				found = true
				activatedAt := m.ActivatedAt
				entry := buildEntry(a)
				entry.CredentialFree = true
				entry.Status = "activated"
				entry.ActivatedAt = &activatedAt
				if m.Alias != "default" {
					entry.Alias = m.Alias
				}
				services = append(services, entry)
			}
			if !found {
				entry := buildEntry(a)
				entry.CredentialFree = true
				entry.Status = "not_activated"
				services = append(services, entry)
			}
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
	oauthCfg.RedirectURL = h.oauthRedirectURL()

	alias := r.URL.Query().Get("alias")
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -, ., @, +)")
		return
	}

	newAccount := r.URL.Query().Get("new_account") == "true"

	mergedScopes, alreadyAuthorized := h.resolveOAuthScopes(r.Context(), user.ID, serviceID, alias, adapter)
	if alreadyAuthorized && !newAccount {
		// Scopes already granted — just ensure service_meta exists and return.
		_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, alias, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{
			"already_authorized": true,
			"service":            serviceID,
		})
		return
	}

	// When adding a new account, use a placeholder alias — the real identity
	// will be resolved from the credential after the OAuth callback completes.
	if newAccount {
		alias = "default"
	}

	// Parse optional config variables from query param.
	var flowConfig map[string]string
	if raw := r.URL.Query().Get("config"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &flowConfig)
	}

	stateToken := uuid.New().String()
	h.oauthStore.StoreOAuth(stateToken, oauthStateEntry{
		UserID:       user.ID,
		ServiceID:    serviceID,
		Alias:        alias,
		PendingReqID: r.URL.Query().Get("pending_request_id"),
		CLICallback:  validateCLICallback(r.URL.Query().Get("cli_callback")),
		Config:       flowConfig,
		Scopes:       mergedScopes,
		TokenPath:    adapterTokenPath(adapter),
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	})

	oauthCfg.Scopes = mergedScopes
	authURL := oauthAuthURL(oauthCfg, stateToken, newAccount || (alias != "" && alias != "default"), adapterScopeParam(adapter))
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
	oauthCfg.RedirectURL = h.oauthRedirectURL()

	alias := r.URL.Query().Get("alias")
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -, ., @, +)")
		return
	}

	mergedScopes, _ := h.resolveOAuthScopes(r.Context(), user.ID, serviceID, alias, adapter)

	var flowConfig map[string]string
	if raw := r.URL.Query().Get("config"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &flowConfig)
	}

	stateToken := uuid.New().String()
	h.oauthStore.StoreOAuth(stateToken, oauthStateEntry{
		UserID:       user.ID,
		ServiceID:    serviceID,
		Alias:        alias,
		PendingReqID: r.URL.Query().Get("pending_request_id"),
		Config:       flowConfig,
		Scopes:       mergedScopes,
		TokenPath:    adapterTokenPath(adapter),
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	})

	oauthCfg.Scopes = mergedScopes
	authURL := oauthAuthURL(oauthCfg, stateToken, alias != "" && alias != "default", adapterScopeParam(adapter))
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

	entry, ok := h.oauthStore.LoadAndDeleteOAuth(state)
	if !ok {
		oauthPopupClose(w, "Invalid or expired OAuth state. Please try again.", "")
		return
	}
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
	oauthCfg.RedirectURL = h.oauthRedirectURL()
	// Use the merged scopes stored during URL generation.
	if len(entry.Scopes) > 0 {
		oauthCfg.Scopes = entry.Scopes
	}

	var credBytes []byte
	alias := entry.Alias
	if alias == "" {
		alias = "default"
	}

	if entry.TokenPath != "" {
		// Non-standard token response (e.g. Slack v2 with user tokens at a nested path).
		// Do a manual exchange and extract the token from the custom path.
		form := url.Values{
			"client_id":     {oauthCfg.ClientID},
			"client_secret": {oauthCfg.ClientSecret},
			"code":          {code},
			"redirect_uri":  {oauthCfg.RedirectURL},
			"grant_type":    {"authorization_code"},
		}
		tokenReq, err := http.NewRequestWithContext(r.Context(), "POST", oauthCfg.Endpoint.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			oauthPopupClose(w, "Failed to build token request.", "")
			return
		}
		tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokenReq.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(tokenReq)
		if err != nil {
			h.logger.Warn("oauth token exchange failed", "service", entry.ServiceID, "err", err)
			oauthPopupClose(w, "Token exchange with provider failed.", "")
			return
		}
		defer resp.Body.Close()

		var rawResp map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&rawResp); err != nil {
			oauthPopupClose(w, "Invalid response from token endpoint.", "")
			return
		}
		if errVal, ok := rawResp["error"].(string); ok && errVal != "" {
			desc, _ := rawResp["error_description"].(string)
			if desc == "" {
				desc = errVal
			}
			h.logger.Warn("oauth token exchange: provider error", "service", entry.ServiceID, "error", errVal, "desc", desc)
			oauthPopupClose(w, "Authorization failed: "+desc, "")
			return
		}

		scopes := entry.Scopes
		if len(scopes) == 0 {
			scopes = adapter.RequiredScopes()
		}
		existingRefreshToken := h.loadExistingRefreshToken(r.Context(), entry.UserID, entry.ServiceID, alias)
		credBytes, err = credentialFromTokenPathResponse(rawResp, entry.TokenPath, scopes, existingRefreshToken, time.Now())
		if err != nil {
			if errors.Is(err, errEmptyAccessToken) {
				h.logger.Warn("oauth token exchange: empty access token", "service", entry.ServiceID, "token_path", entry.TokenPath)
				oauthPopupClose(w, "Provider returned empty access token.", "")
				return
			}
			h.logger.Warn("credential from token_path response failed", "service", entry.ServiceID, "err", err)
			oauthPopupClose(w, "Failed to process credential.", "")
			return
		}
	} else {
		// Standard OAuth2 token exchange.
		token, err := oauthCfg.Exchange(r.Context(), code)
		if err != nil {
			h.logger.Warn("oauth token exchange failed", "service", entry.ServiceID, "err", err)
			oauthPopupClose(w, "Token exchange with provider failed.", "")
			return
		}

		// Use the scopes actually granted by the user. Google returns them in
		// the token exchange response as a space-separated "scope" field. Users
		// can deselect individual scopes on the consent screen, so the granted
		// set may be a subset of what we requested.
		var scopes []string
		scopesGranted := false
		if grantedRaw, ok := token.Extra("scope").(string); ok && grantedRaw != "" {
			scopes = strings.Split(grantedRaw, " ")
			sort.Strings(scopes)
			scopesGranted = true
		} else {
			// Fallback for providers that don't return scope in the token response.
			scopes = entry.Scopes
			if len(scopes) == 0 {
				scopes = adapter.RequiredScopes()
			}
		}

		if token.RefreshToken == "" {
			if existing := h.loadExistingRefreshToken(r.Context(), entry.UserID, entry.ServiceID, alias); existing != "" {
				token.RefreshToken = existing
			}
		}

		credBytes, err = credential.FromToken(token, scopes, scopesGranted)
		if err != nil {
			h.logger.Warn("credential from token failed", "service", entry.ServiceID, "err", err)
			oauthPopupClose(w, "Failed to process credential.", "")
			return
		}
	}

	// Auto-detect identity (e.g. email) before storing so the vault key is correct.
	alias = h.resolveIdentityAlias(r.Context(), entry.ServiceID, alias, credBytes, entry.Config)

	vKey := h.adapterReg.VaultKeyWithAlias(entry.ServiceID, alias)
	if err := h.vault.Set(r.Context(), entry.UserID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Failed to store credential in vault.", "")
		return
	}

	_ = h.st.UpsertServiceMeta(r.Context(), entry.UserID, entry.ServiceID, alias, time.Now())

	// Store per-service config (variable values) if provided during OAuth flow start.
	if len(entry.Config) > 0 {
		configJSON, _ := json.Marshal(entry.Config)
		_ = h.st.UpsertServiceConfig(r.Context(), entry.UserID, entry.ServiceID, alias, configJSON)
	}

	h.logger.Info("service activated", "user", entry.UserID, "service", entry.ServiceID, "alias", alias)

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
%sif(window.opener){try{window.close()}catch(e){}}else{window.location.href='/dashboard/services'}
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
			PendingRequestID string            `json:"pending_request_id"`
			Alias            string            `json:"alias"`
			Config           map[string]string `json:"config,omitempty"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
		alias := body.Alias
		if alias == "" {
			alias = "default"
		}
		if !validAlias(alias) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -, ., @, +)")
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
		h.oauthStore.StoreOAuth(stateToken, oauthStateEntry{
			UserID:       user.ID,
			ServiceID:    serviceID,
			Alias:        alias,
			PendingReqID: body.PendingRequestID,
			Config:       body.Config,
			Scopes:       mergedScopes,
			TokenPath:    adapterTokenPath(adapter),
			ExpiresAt:    time.Now().Add(10 * time.Minute),
		})
		oauthCfg := adapter.OAuthConfig()
		oauthCfg.RedirectURL = h.oauthRedirectURL()
		oauthCfg.Scopes = mergedScopes
		authURL := oauthAuthURL(oauthCfg, stateToken, alias != "" && alias != "default", adapterScopeParam(adapter))
		writeJSON(w, http.StatusOK, map[string]string{"url": authURL})
		return
	}

	// Credential-free service (e.g. iMessage): create service_meta record, no vault op.
	if adapter.ValidateCredential(nil) == nil {
		// If the adapter supports activation checks (e.g. iMessage checking Full
		// Disk Access), run them first. The attempt itself may trigger OS-level
		// permission registration (macOS adds the app to Full Disk Access).
		if ac, ok := adapter.(adapters.ActivationChecker); ok {
			if err := ac.CheckPermissions(); err != nil {
				writeError(w, http.StatusPreconditionFailed, "ACTIVATION_CHECK_FAILED", err.Error())
				return
			}
		}
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
		Token  string            `json:"token"`
		Alias  string            `json:"alias"`
		Config map[string]string `json:"config,omitempty"`
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
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -, ., @, +)")
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

	// Auto-detect identity (e.g. GitHub username) before storing.
	alias = h.resolveIdentityAlias(r.Context(), serviceID, alias, credBytes, body.Config)

	vKey := h.adapterReg.VaultKeyWithAlias(serviceID, alias)
	if err := h.vault.Set(r.Context(), user.ID, vKey, credBytes); err != nil {
		h.logger.Warn("vault set failed (api key)", "service", serviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "VAULT_ERROR", "failed to store credential")
		return
	}
	_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, alias, time.Now())

	// Store per-service config (variable values) if provided.
	if len(body.Config) > 0 {
		configJSON, _ := json.Marshal(body.Config)
		_ = h.st.UpsertServiceConfig(r.Context(), user.ID, serviceID, alias, configJSON)
	}

	h.logger.Info("service activated via api key", "user", user.ID, "service", serviceID, "alias", alias)

	writeJSON(w, http.StatusOK, map[string]string{"status": "activated", "service": serviceID, "alias": alias})
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
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -, ., @, +)")
		return
	}

	// Dry-run mode: return the number of active tasks that would be revoked
	// without actually deactivating.
	if r.URL.Query().Get("dry_run") == "true" {
		count := h.countTasksForService(r.Context(), user.ID, serviceID, alias)
		writeJSON(w, http.StatusOK, map[string]any{
			"service":             serviceID,
			"affected_task_count": count,
		})
		return
	}

	// Remove the service_meta and service_config records first.
	_ = h.st.DeleteServiceMeta(r.Context(), user.ID, serviceID, alias)
	_ = h.st.DeleteServiceConfig(r.Context(), user.ID, serviceID, alias)

	// Credential-free services have no vault credential to clean up.
	adapter, ok := h.adapterReg.Get(serviceID)
	if ok && adapter.ValidateCredential(nil) == nil {
		h.revokeTasksForService(r.Context(), user.ID, serviceID, alias)
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

	h.revokeTasksForService(r.Context(), user.ID, serviceID, alias)

	h.logger.Info("service deactivated", "user", user.ID, "service", serviceID, "alias", alias)

	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated", "service": serviceID})
}

// serviceMatchStrings returns the base service and optional service:alias
// strings used to match tasks against a service+alias pair.
func serviceMatchStrings(serviceID, alias string) (base, withAlias string) {
	base = serviceID
	if alias != "" && alias != "default" {
		withAlias = serviceID + ":" + alias
	}
	return
}

// countTasksForService returns the number of active tasks that reference the
// given service and alias.
func (h *ServicesHandler) countTasksForService(ctx context.Context, userID, serviceID, alias string) int {
	tasks, _, err := h.st.ListTasks(ctx, userID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		h.logger.Warn("failed to list tasks for service count", "err", err, "user", userID)
		return 0
	}
	base, withAlias := serviceMatchStrings(serviceID, alias)
	count := 0
	for _, t := range tasks {
		if taskReferencesService(t, base, withAlias) {
			count++
		}
	}
	return count
}

// revokeTasksForService revokes all active tasks that have authorized actions
// referencing the given service and alias.
func (h *ServicesHandler) revokeTasksForService(ctx context.Context, userID, serviceID, alias string) {
	tasks, _, err := h.st.ListTasks(ctx, userID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		h.logger.Warn("failed to list tasks for service cleanup", "err", err, "user", userID)
		return
	}

	base, withAlias := serviceMatchStrings(serviceID, alias)

	for _, t := range tasks {
		if taskReferencesService(t, base, withAlias) {
			if err := h.st.RevokeTask(ctx, t.ID, userID); err != nil {
				h.logger.Warn("failed to revoke task for deactivated service", "err", err, "task_id", t.ID)
				continue
			}
			_ = h.st.DeleteChainFactsByTask(ctx, t.ID)
			h.logger.Info("revoked task due to service deactivation", "task_id", t.ID, "service", serviceID, "alias", alias)
		}
	}

	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "tasks"})
		h.eventHub.Publish(userID, events.Event{Type: "queue"})
	}
}

// taskReferencesService checks whether any authorized action in the task
// references the given service. For default alias, it matches actions with
// the base service type. For named aliases, it matches the "service:alias" form.
func taskReferencesService(t *store.Task, baseService, serviceWithAlias string) bool {
	for _, a := range t.AuthorizedActions {
		if serviceWithAlias != "" {
			// Named alias: only match exact "service:alias".
			if a.Service == serviceWithAlias {
				return true
			}
		} else {
			// Default alias: match base service name (no colon suffix)
			// or explicit "service:default".
			if a.Service == baseService || a.Service == baseService+":default" {
				return true
			}
		}
	}
	return false
}

// RenameAlias renames an existing service connection alias.
//
// POST /api/services/{serviceID}/rename-alias
// Auth: user JWT
// Body: {"old_alias": "...", "new_alias": "..."}
func (h *ServicesHandler) RenameAlias(w http.ResponseWriter, r *http.Request) {
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
		OldAlias string `json:"old_alias"`
		NewAlias string `json:"new_alias"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}
	if body.OldAlias == "" {
		body.OldAlias = "default"
	}
	if body.NewAlias == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "new_alias is required")
		return
	}
	if !validAlias(body.NewAlias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "new_alias contains invalid characters (allowed: a-z, A-Z, 0-9, _, -, ., @, +)")
		return
	}
	if body.OldAlias == body.NewAlias {
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "service": serviceID, "alias": body.NewAlias})
		return
	}

	// Verify the old alias exists.
	oldMeta, err := h.st.GetServiceMeta(r.Context(), user.ID, serviceID, body.OldAlias)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "connection not found")
		return
	}

	// Check the new alias doesn't already exist for this service.
	if _, err := h.st.GetServiceMeta(r.Context(), user.ID, serviceID, body.NewAlias); err == nil {
		writeError(w, http.StatusConflict, "CONFLICT", "a connection with that alias already exists")
		return
	}

	// Move the vault credential (if the service has one).
	oldVKey := h.adapterReg.VaultKeyWithAlias(serviceID, body.OldAlias)
	newVKey := h.adapterReg.VaultKeyWithAlias(serviceID, body.NewAlias)
	if oldVKey != newVKey {
		credBytes, err := h.vault.Get(r.Context(), user.ID, oldVKey)
		if err == nil && len(credBytes) > 0 {
			if err := h.vault.Set(r.Context(), user.ID, newVKey, credBytes); err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to move credential")
				return
			}
			_ = h.vault.Delete(r.Context(), user.ID, oldVKey)
		}
	}

	// Rename this service's meta.
	_ = h.st.UpsertServiceMeta(r.Context(), user.ID, serviceID, body.NewAlias, oldMeta.ActivatedAt)
	_ = h.st.DeleteServiceMeta(r.Context(), user.ID, serviceID, body.OldAlias)

	// If other services share the same vault key (e.g. all Google services share "google"),
	// rename their metas too so they continue to find the credential.
	metas, _ := h.st.ListServiceMetas(r.Context(), user.ID)
	for _, m := range metas {
		if m.ServiceID == serviceID {
			continue // already handled
		}
		if h.adapterReg.VaultKeyWithAlias(m.ServiceID, m.Alias) == oldVKey {
			_ = h.st.UpsertServiceMeta(r.Context(), user.ID, m.ServiceID, body.NewAlias, m.ActivatedAt)
			_ = h.st.DeleteServiceMeta(r.Context(), user.ID, m.ServiceID, m.Alias)
			h.logger.Info("alias renamed (shared key)", "user", user.ID, "service", m.ServiceID, "old", m.Alias, "new", body.NewAlias)
		}
	}

	h.logger.Info("alias renamed", "user", user.ID, "service", serviceID, "old", body.OldAlias, "new", body.NewAlias)
	writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "service": serviceID, "alias": body.NewAlias})
}

// resolveIdentityAlias attempts to auto-detect the account identity for a service.
// If the adapter implements IdentityFetcher and the current alias is "default",
// it fetches the identity and returns it as the new alias. On failure or if the
// adapter doesn't support identity fetching, it returns the original alias unchanged.
func (h *ServicesHandler) resolveIdentityAlias(
	ctx context.Context, serviceID, currentAlias string, credBytes []byte, config map[string]string,
) string {
	if currentAlias != "default" {
		return currentAlias // user explicitly chose an alias
	}

	adapter, ok := h.adapterReg.Get(serviceID)
	if !ok {
		return currentAlias
	}

	fetcher, ok := adapter.(adapters.IdentityFetcher)
	if !ok {
		return currentAlias
	}

	identity, err := fetcher.FetchIdentity(ctx, credBytes, config)
	if err != nil || identity == "" {
		if err != nil {
			h.logger.Warn("identity fetch failed, using default alias",
				"service", serviceID, "err", err)
		}
		return currentAlias
	}

	if !validAlias(identity) {
		h.logger.Warn("fetched identity contains invalid characters, using default alias",
			"service", serviceID, "identity", identity)
		return currentAlias
	}

	h.logger.Info("auto-detected service identity",
		"service", serviceID, "identity", identity)
	return identity
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
	result, execErr := executeAdapterRequest(ctx, h.vault, h.adapterReg, h.st,
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
	h.oauthStore.Cleanup()
}

// adapterScopeParam returns the custom OAuth scope parameter name (e.g.
// "user_scope" for Slack v2) if the adapter declares one, or "" for default.
func adapterScopeParam(a adapters.Adapter) string {
	type scopeParamer interface{ OAuthScopeParam() string }
	if sp, ok := a.(scopeParamer); ok {
		return sp.OAuthScopeParam()
	}
	return ""
}

// adapterTokenPath returns the custom token path (e.g.
// "authed_user.access_token" for Slack v2) if the adapter declares one.
func adapterTokenPath(a adapters.Adapter) string {
	type tokenPather interface{ OAuthTokenPath() string }
	if tp, ok := a.(tokenPather); ok {
		return tp.OAuthTokenPath()
	}
	return ""
}

// oauthAuthURL builds the OAuth2 authorization URL. When selectAccount is true
// (multi-account or new_account flow), it adds prompt=consent select_account
// so the user can choose a different Google account.
//
// scopeParam overrides the query parameter name for scopes (e.g. "user_scope"
// for Slack v2). When empty, the default "scope" parameter is used.
func oauthAuthURL(cfg *oauth2.Config, stateToken string, selectAccount bool, scopeParam string) string {
	opts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
	}
	// Google-specific parameters: include_granted_scopes for incremental
	// authorization, and prompt for account selection. Other providers
	// (e.g. Dropbox) reject these.
	if strings.Contains(cfg.Endpoint.AuthURL, "google.com") {
		prompt := "consent"
		if selectAccount {
			prompt = "consent select_account"
		}
		opts = append(opts,
			oauth2.SetAuthURLParam("include_granted_scopes", "true"),
			oauth2.SetAuthURLParam("prompt", prompt),
		)
	}
	if scopeParam != "" && scopeParam != "scope" {
		// Move scopes to the custom parameter (e.g. Slack v2 "user_scope")
		// and clear the default so AuthCodeURL doesn't emit both.
		opts = append(opts, oauth2.SetAuthURLParam(scopeParam, strings.Join(cfg.Scopes, " ")))
		cfg = &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     cfg.Endpoint,
			RedirectURL:  cfg.RedirectURL,
		}
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

// ── Device Flow (RFC 8628) ───────────────────────────────────────────────────

type deviceFlowEntry struct {
	UserID     string
	ServiceID  string
	Alias      string
	Config     map[string]string // per-service variable values (may be nil)
	DeviceCode string
	ClientID   string
	TokenURL   string
	GrantType  string
	Interval   int
	ExpiresAt  time.Time
}

// DeviceFlowStart initiates a device authorization flow.
//
// POST /api/services/{serviceID}/device-flow/start
// Auth: user JWT
// Body: {"alias": "..."} (optional)
// Response: {"flow_id": "...", "user_code": "ABCD-1234", "verification_uri": "...", "interval": 5, "expires_in": 900}
func (h *ServicesHandler) DeviceFlowStart(w http.ResponseWriter, r *http.Request) {
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

	dfp, ok := adapter.(adapters.DeviceFlowProvider)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service does not support device flow")
		return
	}
	dfCfg := dfp.DeviceFlowConfig()
	if dfCfg == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "device flow not configured (missing client_id)")
		return
	}

	var body struct {
		Alias  string            `json:"alias"`
		Config map[string]string `json:"config,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	alias := body.Alias
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters")
		return
	}

	// Resolve client_id.
	clientID := dfCfg.ClientID
	if dfCfg.ClientIDEnv != "" {
		if v := os.Getenv(dfCfg.ClientIDEnv); v != "" {
			clientID = v
		}
	}

	// Request device code from the provider.
	form := url.Values{
		"client_id": {clientID},
		"scope":     {strings.Join(dfCfg.Scopes, " ")},
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", dfCfg.DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to build request")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.logger.Warn("device flow: request to provider failed", "err", err)
		writeError(w, http.StatusBadGateway, "PROVIDER_ERROR", "failed to contact provider")
		return
	}
	defer resp.Body.Close()

	var dfResp struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		ErrorDesc       string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dfResp); err != nil {
		writeError(w, http.StatusBadGateway, "PROVIDER_ERROR", "invalid response from provider")
		return
	}
	if dfResp.Error != "" {
		h.logger.Warn("device flow: provider error", "error", dfResp.Error, "desc", dfResp.ErrorDesc)
		writeError(w, http.StatusBadGateway, "PROVIDER_ERROR", dfResp.ErrorDesc)
		return
	}

	grantType := dfCfg.GrantType
	if grantType == "" {
		grantType = "urn:ietf:params:oauth:grant-type:device_code"
	}
	interval := dfResp.Interval
	if interval < 5 {
		interval = 5
	}

	flowID := uuid.New().String()
	h.oauthStore.StoreDeviceFlow(flowID, deviceFlowEntry{
		UserID:     user.ID,
		ServiceID:  serviceID,
		Alias:      alias,
		Config:     body.Config,
		DeviceCode: dfResp.DeviceCode,
		ClientID:   clientID,
		TokenURL:   dfCfg.TokenURL,
		GrantType:  grantType,
		Interval:   interval,
		ExpiresAt:  time.Now().Add(time.Duration(dfResp.ExpiresIn) * time.Second),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":          flowID,
		"user_code":        dfResp.UserCode,
		"verification_uri": dfResp.VerificationURI,
		"interval":         interval,
		"expires_in":       dfResp.ExpiresIn,
	})
}

// DeviceFlowPoll polls for device flow completion.
//
// POST /api/services/{serviceID}/device-flow/poll
// Auth: user JWT
// Body: {"flow_id": "..."}
// Response: {"status": "pending|complete|expired|denied|slow_down", "interval": ...}
func (h *ServicesHandler) DeviceFlowPoll(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var body struct {
		FlowID string `json:"flow_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.FlowID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "flow_id is required")
		return
	}

	entry, ok := h.oauthStore.LoadDeviceFlow(body.FlowID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown or expired flow")
		return
	}

	if entry.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "flow does not belong to this user")
		return
	}
	if time.Now().After(entry.ExpiresAt) {
		h.oauthStore.DeleteDeviceFlow(body.FlowID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}

	// Poll the provider's token endpoint.
	form := url.Values{
		"client_id":   {entry.ClientID},
		"device_code": {entry.DeviceCode},
		"grant_type":  {entry.GrantType},
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", entry.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to build request")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.logger.Warn("device flow poll: request failed", "err", err)
		writeError(w, http.StatusBadGateway, "PROVIDER_ERROR", "failed to contact provider")
		return
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		writeError(w, http.StatusBadGateway, "PROVIDER_ERROR", "invalid response from provider")
		return
	}

	switch tokenResp.Error {
	case "authorization_pending":
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	case "slow_down":
		// Increase interval by 5 seconds per spec.
		entry.Interval += 5
		h.oauthStore.UpdateDeviceFlow(body.FlowID, entry)
		writeJSON(w, http.StatusOK, map[string]any{"status": "slow_down", "interval": entry.Interval})
		return
	case "expired_token":
		h.oauthStore.DeleteDeviceFlow(body.FlowID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	case "access_denied":
		h.oauthStore.DeleteDeviceFlow(body.FlowID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "denied"})
		return
	case "":
		// Success — fall through.
	default:
		h.oauthStore.DeleteDeviceFlow(body.FlowID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": tokenResp.Error})
		return
	}

	// Success: validate and store the token as an api_key credential.
	if tokenResp.AccessToken == "" {
		h.logger.Warn("device flow: provider returned empty access token", "service", entry.ServiceID)
		h.oauthStore.DeleteDeviceFlow(body.FlowID)
		writeError(w, http.StatusBadGateway, "PROVIDER_ERROR", "provider returned empty access token")
		return
	}
	credBytes, err := json.Marshal(map[string]string{
		"type":  "api_key",
		"token": tokenResp.AccessToken,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to encode credential")
		return
	}

	// Auto-detect identity before storing.
	alias := h.resolveIdentityAlias(r.Context(), entry.ServiceID, entry.Alias, credBytes, entry.Config)

	vKey := h.adapterReg.VaultKeyWithAlias(entry.ServiceID, alias)
	if err := h.vault.Set(r.Context(), entry.UserID, vKey, credBytes); err != nil {
		h.logger.Warn("device flow: vault set failed", "err", err)
		writeError(w, http.StatusInternalServerError, "VAULT_ERROR", "failed to store credential")
		return
	}
	_ = h.st.UpsertServiceMeta(r.Context(), entry.UserID, entry.ServiceID, alias, time.Now())
	if len(entry.Config) > 0 {
		configJSON, _ := json.Marshal(entry.Config)
		_ = h.st.UpsertServiceConfig(r.Context(), entry.UserID, entry.ServiceID, alias, configJSON)
	}
	h.oauthStore.DeleteDeviceFlow(body.FlowID)

	h.logger.Info("service activated via device flow", "user", entry.UserID, "service", entry.ServiceID, "alias", alias)
	writeJSON(w, http.StatusOK, map[string]any{"status": "complete", "alias": alias})
}

// ── PKCE Flow (RFC 7636) ─────────────────────────────────────────────────────

type pkceFlowEntry struct {
	UserID       string
	ServiceID    string
	Alias        string
	Config       map[string]string // per-service variable values (may be nil)
	CodeVerifier string
	CLICallback  string
	TokenURL     string
	TokenPath    string // JSON path to access token (e.g. "authed_user.access_token")
	ClientID     string
	RedirectURI  string
	ExpiresAt    time.Time
}

// PKCEFlowStart initiates a PKCE authorization code flow.
//
// POST /api/services/{serviceID}/pkce-flow/start
// Auth: user JWT
// Body: {"alias": "...", "cli_callback": "http://127.0.0.1:PORT/oauth-done"}
// Response: {"authorize_url": "https://...", "state": "..."}
func (h *ServicesHandler) PKCEFlowStart(w http.ResponseWriter, r *http.Request) {
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

	pfp, ok := adapter.(adapters.PKCEFlowProvider)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service does not support PKCE flow")
		return
	}

	var body struct {
		Alias       string            `json:"alias"`
		CLICallback string            `json:"cli_callback"`
		ClientID    string            `json:"client_id"`
		Config      map[string]string `json:"config,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	alias := body.Alias
	if alias == "" {
		alias = "default"
	}
	if !validAlias(alias) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "alias contains invalid characters")
		return
	}

	// If a client_id was provided in the request, save it to the vault for future use.
	if body.ClientID != "" {
		if err := adapters.SetPKCEClientID(r.Context(), h.vault, serviceID, body.ClientID); err != nil {
			h.logger.Error("failed to store PKCE client ID", "service_id", serviceID, "err", err)
		}
	}

	// Get the PKCE flow config. PKCEFlowConfig() returns nil if no client_id is
	// resolvable from env/yaml, but we may have one from the vault or from the request body.
	pfCfg := pfp.PKCEFlowConfig()

	// If PKCEFlowConfig returned nil (no hardcoded/env client ID), we still have a
	// PKCE flow if the YAML defines one — get it directly from the adapter def.
	if pfCfg == nil {
		if mp, ok2 := adapter.(adapters.MetadataProvider); ok2 {
			meta := mp.ServiceMetadata()
			if !meta.PKCEFlowDefined {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "PKCE flow not configured for this service")
				return
			}
		}
		// Get the raw def to access PKCE config without client ID check.
		type defHolder interface{ Def() yamldef.ServiceDef }
		if dh, ok2 := adapter.(defHolder); ok2 && dh.Def().Auth.PKCEFlow != nil {
			pfCfg = dh.Def().Auth.PKCEFlow
		} else {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "PKCE flow not configured")
			return
		}
	}

	// Resolve client_id: request body → env var → vault → hardcoded.
	clientID := body.ClientID
	if clientID == "" {
		if pfCfg.ClientIDEnv != "" {
			if v := os.Getenv(pfCfg.ClientIDEnv); v != "" {
				clientID = v
			}
		}
	}
	if clientID == "" {
		clientID = adapters.GetPKCEClientID(r.Context(), h.vault, serviceID)
	}
	if clientID == "" {
		clientID = pfCfg.ClientID
	}
	if clientID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "PKCE flow requires a client_id — provide one in the request or set the "+pfCfg.ClientIDEnv+" environment variable")
		return
	}

	// Generate PKCE code verifier and challenge.
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate PKCE verifier")
		return
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeHash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	stateToken := uuid.New().String()

	// Some providers (e.g. Slack) require HTTPS redirect URIs — use the relay
	// daemon URL. Others (e.g. Linear) accept http://localhost — use the
	// local base URL to avoid relay path issues.
	redirectBase := h.relayDaemonURL
	if redirectBase == "" || pfCfg.LocalhostRedirect {
		redirectBase = h.baseURL
	}
	redirectURI := redirectBase + "/api/pkce-flow/callback"

	h.oauthStore.StorePKCE(stateToken, pkceFlowEntry{
		UserID:       user.ID,
		ServiceID:    serviceID,
		Alias:        alias,
		Config:       body.Config,
		CodeVerifier: codeVerifier,
		CLICallback:  validateCLICallback(body.CLICallback),
		TokenURL:     pfCfg.TokenURL,
		TokenPath:    pfCfg.TokenPath,
		ClientID:     clientID,
		RedirectURI:  redirectURI,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	})

	// Build authorize URL.
	params := url.Values{
		"client_id":             {clientID},
		"scope":                 {strings.Join(pfCfg.Scopes, " ")},
		"redirect_uri":          {redirectURI},
		"state":                 {stateToken},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
	}
	// Slack PKCE requires user_scope instead of scope for user tokens.
	if strings.Contains(pfCfg.AuthorizeURL, "slack.com") {
		params.Del("scope")
		params.Set("user_scope", strings.Join(pfCfg.Scopes, " "))
	}
	authorizeURL := pfCfg.AuthorizeURL + "?" + params.Encode()

	writeJSON(w, http.StatusOK, map[string]string{
		"authorize_url": authorizeURL,
		"state":         stateToken,
	})
}

// PKCEFlowCallback handles the OAuth redirect after user authorization.
// It exchanges the authorization code + PKCE verifier for an access token,
// stores the credential, and serves a success/error HTML page.
//
// GET /api/pkce-flow/callback?code=...&state=...
func (h *ServicesHandler) PKCEFlowCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	if state == "" || code == "" {
		oauthPopupClose(w, "Missing PKCE callback parameters.", "")
		return
	}

	entry, ok := h.oauthStore.LoadAndDeletePKCE(state)
	if !ok {
		oauthPopupClose(w, "Invalid or expired PKCE state. Please try again.", "")
		return
	}
	if time.Now().After(entry.ExpiresAt) {
		oauthPopupClose(w, "PKCE session expired. Please try again.", "")
		return
	}

	adapter, ok := h.adapterReg.Get(entry.ServiceID)
	if !ok {
		oauthPopupClose(w, "Service not found.", "")
		return
	}
	alias := entry.Alias
	if alias == "" {
		alias = "default"
	}

	// Exchange authorization code for access token using PKCE.
	form := url.Values{
		"client_id":     {entry.ClientID},
		"code":          {code},
		"code_verifier": {entry.CodeVerifier},
		"redirect_uri":  {entry.RedirectURI},
		"grant_type":    {"authorization_code"},
	}
	tokenReq, err := http.NewRequestWithContext(r.Context(), "POST", entry.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		oauthPopupClose(w, "Failed to build token request.", "")
		return
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		h.logger.Warn("pkce flow: token exchange failed", "err", err)
		oauthPopupClose(w, "Failed to contact token endpoint.", "")
		return
	}
	defer resp.Body.Close()

	var rawResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rawResp); err != nil {
		oauthPopupClose(w, "Invalid response from token endpoint.", "")
		return
	}

	// Check for error in response.
	if errVal, ok := rawResp["error"].(string); ok && errVal != "" {
		desc, _ := rawResp["error_description"].(string)
		if desc == "" {
			desc = errVal
		}
		h.logger.Warn("pkce flow: provider returned error", "error", errVal, "desc", desc)
		oauthPopupClose(w, "Authorization failed: "+desc, "")
		return
	}

	credBytes, err := credentialFromTokenPathResponse(
		rawResp,
		entry.TokenPath,
		adapter.RequiredScopes(),
		h.loadExistingRefreshToken(r.Context(), entry.UserID, entry.ServiceID, alias),
		time.Now(),
	)
	if err != nil {
		if errors.Is(err, errEmptyAccessToken) {
			h.logger.Warn("pkce flow: empty access token in response", "service", entry.ServiceID, "token_path", entry.TokenPath)
			oauthPopupClose(w, "Provider returned empty access token.", "")
			return
		}
		h.logger.Warn("pkce flow: failed to process token response", "service", entry.ServiceID, "err", err)
		oauthPopupClose(w, "Failed to process credential.", "")
		return
	}

	// Auto-detect identity before storing.
	alias = h.resolveIdentityAlias(r.Context(), entry.ServiceID, alias, credBytes, entry.Config)

	vKey := h.adapterReg.VaultKeyWithAlias(entry.ServiceID, alias)
	if err := h.vault.Set(r.Context(), entry.UserID, vKey, credBytes); err != nil {
		h.logger.Warn("pkce flow: vault set failed", "err", err)
		oauthPopupClose(w, "Failed to store credential.", "")
		return
	}
	_ = h.st.UpsertServiceMeta(r.Context(), entry.UserID, entry.ServiceID, alias, time.Now())
	if len(entry.Config) > 0 {
		configJSON, _ := json.Marshal(entry.Config)
		_ = h.st.UpsertServiceConfig(r.Context(), entry.UserID, entry.ServiceID, alias, configJSON)
	}

	h.logger.Info("service activated via PKCE flow", "user", entry.UserID, "service", entry.ServiceID, "alias", alias)
	oauthPopupClose(w, "", entry.CLICallback)
}

// extractTokenFromPath navigates a nested map using a dot-separated path
// and returns the string value at that path.
func extractTokenFromPath(m map[string]any, path string) string {
	if path == "" {
		// Default: look for "access_token" at the top level.
		return extractStringFromPath(m, "access_token")
	}
	return extractStringFromPath(m, path)
}

func credentialFromTokenPathResponse(rawResp map[string]any, tokenPath string, scopes []string, existingRefreshToken string, now time.Time) ([]byte, error) {
	accessToken := extractTokenFromPath(rawResp, tokenPath)
	if accessToken == "" {
		return nil, errEmptyAccessToken
	}

	refreshToken := extractStringFromPath(rawResp, siblingTokenFieldPath(tokenPath, "refresh_token"))
	if refreshToken == "" {
		refreshToken = existingRefreshToken
	}

	token := &oauth2.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
	}
	if expiresIn, ok := extractIntFromPath(rawResp, siblingTokenFieldPath(tokenPath, "expires_in")); ok && expiresIn > 0 {
		token.Expiry = now.Add(time.Duration(expiresIn) * time.Second)
	}
	return credential.FromToken(token, scopes, false)
}

func extractStringFromPath(m map[string]any, path string) string {
	parts := strings.Split(path, ".")
	var current any = m
	for _, part := range parts {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = obj[part]
	}
	if s, ok := current.(string); ok {
		return s
	}
	return ""
}

func extractIntFromPath(m map[string]any, path string) (int, bool) {
	parts := strings.Split(path, ".")
	var current any = m
	for _, part := range parts {
		obj, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current = obj[part]
	}
	switch v := current.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

func siblingTokenFieldPath(tokenPath, field string) string {
	if tokenPath == "" {
		return field
	}
	lastDot := strings.LastIndex(tokenPath, ".")
	if lastDot == -1 {
		return field
	}
	return tokenPath[:lastDot+1] + field
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

// ListPKCECredentials returns all configured PKCE client IDs.
//
// GET /api/system/pkce-credentials
// Auth: user JWT
// Response: [{"service_id": "...", "client_id": "..."}]
func (h *ServicesHandler) ListPKCECredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := adapters.ListPKCEClientIDs(r.Context(), h.vault)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list PKCE credentials")
		return
	}
	type entry struct {
		ServiceID string `json:"service_id"`
		ClientID  string `json:"client_id"`
	}
	result := make([]entry, 0, len(creds))
	for sid, cid := range creds {
		result = append(result, entry{ServiceID: sid, ClientID: cid})
	}
	writeJSON(w, http.StatusOK, result)
}

// SetPKCECredential stores a PKCE client ID for a specific service.
//
// POST /api/system/pkce-credentials
// Auth: user JWT
// Body: {"service_id": "...", "client_id": "..."}
// Response: {"ok": true}
func (h *ServicesHandler) SetPKCECredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServiceID string `json:"service_id"`
		ClientID  string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return
	}
	if body.ServiceID == "" || body.ClientID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service_id and client_id are required")
		return
	}

	if err := adapters.SetPKCEClientID(r.Context(), h.vault, body.ServiceID, body.ClientID); err != nil {
		h.logger.Error("failed to store PKCE client ID", "service_id", body.ServiceID, "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to store credential")
		return
	}

	h.logger.Info("PKCE client ID stored", "service_id", body.ServiceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DeletePKCECredential removes a PKCE client ID for a specific service.
//
// DELETE /api/system/pkce-credentials/{service_id}
// Auth: user JWT
// Response: {"ok": true}
func (h *ServicesHandler) DeletePKCECredential(w http.ResponseWriter, r *http.Request) {
	serviceID := r.PathValue("service_id")
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service_id is required")
		return
	}

	if err := adapters.DeletePKCEClientID(r.Context(), h.vault, serviceID); err != nil {
		h.logger.Error("failed to delete PKCE client ID", "service_id", serviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to delete credential")
		return
	}

	h.logger.Info("PKCE client ID removed", "service_id", serviceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
