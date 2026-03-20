package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/relay"
	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Provider implements OAuth 2.1 endpoints for MCP client authentication.
type Provider struct {
	st      store.Store
	jwtSvc  pkgauth.TokenService
	baseURL string
	logger  *slog.Logger
}

// NewProvider creates an OAuth provider.
func NewProvider(st store.Store, jwtSvc pkgauth.TokenService, baseURL string, logger *slog.Logger) *Provider {
	return &Provider{st: st, jwtSvc: jwtSvc, baseURL: baseURL, logger: logger}
}

// Register handles POST /oauth/register (RFC 7591 Dynamic Client Registration).
func (p *Provider) Register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if req.ClientName == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_name is required")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uris is required")
		return
	}
	for _, uri := range req.RedirectURIs {
		if err := validateRedirectURI(uri); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}

	client := &store.OAuthClient{
		ID:           uuid.New().String(),
		ClientName:   req.ClientName,
		RedirectURIs: req.RedirectURIs,
	}
	if err := p.st.CreateOAuthClient(r.Context(), client); err != nil {
		p.logger.Error("failed to create oauth client", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to register client")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"client_id":     client.ID,
		"client_name":   client.ClientName,
		"redirect_uris": client.RedirectURIs,
	})
}

// AuthorizeApprove handles POST /oauth/authorize (user approves the consent).
// Expects a JSON body with the OAuth params + a valid user JWT in the Authorization header.
func (p *Provider) AuthorizeApprove(w http.ResponseWriter, r *http.Request) {
	// Authenticate user via JWT.
	token := bearerToken(r)
	if token == "" {
		writeOAuthError(w, http.StatusUnauthorized, "access_denied", "authentication required")
		return
	}
	claims, err := p.jwtSvc.ValidateToken(token)
	if err != nil || claims.Purpose != "" {
		writeOAuthError(w, http.StatusUnauthorized, "access_denied", "invalid or expired token")
		return
	}

	var req struct {
		ClientID      string `json:"client_id"`
		RedirectURI   string `json:"redirect_uri"`
		State         string `json:"state"`
		CodeChallenge string `json:"code_challenge"`
		Scope         string `json:"scope"`
		DaemonID      string `json:"daemon_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	// Validate redirect URI scheme (defense-in-depth).
	if err := validateRedirectURI(req.RedirectURI); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Validate client + redirect URI.
	client, err := p.st.GetOAuthClient(r.Context(), req.ClientID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "unknown client_id")
		return
	}
	if !uriRegistered(client.RedirectURIs, req.RedirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri not registered")
		return
	}

	// Generate authorization code.
	codeBytes := make([]byte, 32)
	if _, err := rand.Read(codeBytes); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate code")
		return
	}
	code := hex.EncodeToString(codeBytes)

	// Store hashed code.
	authCode := &store.OAuthAuthorizationCode{
		CodeHash:      auth.HashToken(code),
		ClientID:      req.ClientID,
		UserID:        claims.UserID,
		DaemonID:      req.DaemonID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Scope:         req.Scope,
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	}
	if err := p.st.SaveAuthorizationCode(r.Context(), authCode); err != nil {
		p.logger.Error("failed to save authorization code", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to save authorization code")
		return
	}

	// Return the redirect URL for the frontend to navigate to.
	redirectURL, _ := url.Parse(req.RedirectURI)
	q := redirectURL.Query()
	q.Set("code", code)
	if req.State != "" {
		q.Set("state", req.State)
	}
	redirectURL.RawQuery = q.Encode()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"redirect_uri": redirectURL.String(),
	})
}

// AuthorizeDeny handles POST /oauth/deny (user denies consent).
// Validates the redirect URI against the registered client before redirecting.
func (p *Provider) AuthorizeDeny(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientID    string `json:"client_id"`
		RedirectURI string `json:"redirect_uri"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	// Validate redirect URI scheme (defense-in-depth).
	if err := validateRedirectURI(req.RedirectURI); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Validate client + redirect URI to prevent open redirect.
	client, err := p.st.GetOAuthClient(r.Context(), req.ClientID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "unknown client_id")
		return
	}
	if !uriRegistered(client.RedirectURIs, req.RedirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri not registered")
		return
	}

	redirectURL, _ := url.Parse(req.RedirectURI)
	q := redirectURL.Query()
	q.Set("error", "access_denied")
	if req.State != "" {
		q.Set("state", req.State)
	}
	redirectURL.RawQuery = q.Encode()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"redirect_uri": redirectURL.String(),
	})
}

// Token handles POST /oauth/token (authorization code exchange or relay pairing).
func (p *Provider) Token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}

	grantType := r.FormValue("grant_type")

	if grantType == "relay_pairing" {
		p.handleRelayPairing(w, r)
		return
	}

	code := r.FormValue("code")
	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	codeVerifier := r.FormValue("code_verifier")

	if grantType != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported grant_type")
		return
	}
	if code == "" || clientID == "" || redirectURI == "" || codeVerifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing required parameters")
		return
	}

	// Atomically consume the authorization code (one-time use).
	codeHash := auth.HashToken(code)
	authCode, err := p.st.ConsumeAuthorizationCode(r.Context(), codeHash)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}

	// Validate code hasn't expired.
	if time.Now().After(authCode.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	}

	// Validate client_id and redirect_uri match.
	if authCode.ClientID != clientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if authCode.RedirectURI != redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}

	// Validate PKCE.
	if !VerifyCodeChallenge(codeVerifier, authCode.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	// Generate an agent token (the access token IS a cvis_ agent token).
	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate token")
		return
	}

	// Look up client name for the agent name.
	client, _ := p.st.GetOAuthClient(r.Context(), clientID)
	agentName := "MCP Client"
	if client != nil {
		agentName = client.ClientName + " (MCP)"
	}

	_, err = p.st.CreateAgent(r.Context(), authCode.UserID, agentName, auth.HashToken(rawToken))
	if err != nil {
		p.logger.Error("failed to create agent for oauth token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to create agent")
		return
	}

	resp := map[string]string{
		"access_token": rawToken,
		"token_type":   "Bearer",
	}
	if authCode.DaemonID != "" {
		resp["daemon_id"] = authCode.DaemonID
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// handleRelayPairing creates an agent and issues a cvis_ token directly.
// This grant type is only accepted when the request arrives via the relay tunnel.
func (p *Provider) handleRelayPairing(w http.ResponseWriter, r *http.Request) {
	if !relay.ViaRelay(r.Context()) {
		writeOAuthError(w, http.StatusForbidden, "access_denied", "relay_pairing grant is only available via relay tunnel")
		return
	}

	// Look up the local user — in daemon mode this is always admin@local.
	user, err := p.st.GetUserByEmail(r.Context(), "admin@local")
	if err != nil {
		p.logger.Error("relay_pairing: could not find local user", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not resolve local user")
		return
	}

	// Generate a long-lived agent token (same as authorization_code grant).
	token, err := auth.GenerateAgentToken()
	if err != nil {
		p.logger.Error("relay_pairing: failed to generate agent token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to generate token")
		return
	}

	tokenHash := auth.HashToken(token)
	agentName := "mcp-relay-agent"

	if _, err := p.st.CreateAgent(r.Context(), user.ID, agentName, tokenHash); err != nil {
		p.logger.Error("relay_pairing: failed to create agent", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to create agent")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
	})
}

// dangerousSchemes are URI schemes that can execute code in a browser context
// when assigned to window.location.href. Custom app schemes (e.g. claude://)
// are safe — they trigger OS protocol handlers per RFC 8252.
var dangerousSchemes = map[string]bool{
	"javascript": true,
	"data":       true,
	"vbscript":   true,
	"blob":       true,
}

// validateRedirectURI rejects redirect URIs with dangerous schemes.
func validateRedirectURI(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("invalid redirect URI: %w", err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("redirect URI must include a scheme")
	}
	if dangerousSchemes[strings.ToLower(u.Scheme)] {
		return fmt.Errorf("redirect URI scheme %q is not allowed", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("redirect URI must include a host")
	}
	return nil
}

// uriRegistered checks whether uri is in the registered list.
func uriRegistered(registered []string, uri string) bool {
	for _, r := range registered {
		if r == uri {
			return true
		}
	}
	return false
}

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

// writeOAuthError writes an RFC 6749 error response.
func writeOAuthError(w http.ResponseWriter, status int, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error":             errCode,
		"error_description": description,
	})
}
