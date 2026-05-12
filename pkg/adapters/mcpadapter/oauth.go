package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// SetOAuthVault wires the vault that holds per-service OAuth client
// credentials (under "__system__"/"mcp.oauth.{serviceID}"). Called from
// defaults.go after LoadFromFS so MCPAdapters can lazily fetch their
// client_id / client_secret without holding a reference at construction time.
//
// This mirrors how YAMLAdapter.SetOAuthProvider is wired for Google services.
func (a *MCPAdapter) SetOAuthVault(v vault.Vault) {
	a.oauthVault = v
}

// oauthSpec returns the spec's OAuth declaration if any. Nil means the
// service uses no OAuth (API key, bearer, or none).
func (a *MCPAdapter) oauthSpec() *MCPOAuthSpec {
	if a.cfg.Spec == nil {
		return nil
	}
	return a.cfg.Spec.MCP.OAuth
}

// OAuthClientCredentials satisfies the adapters.OAuthCredentialProvider
// interface (which has no ctx parameter). It delegates to the context-aware
// variant with Background for the rare interface-only call sites.
// Internal callers that have a context should use oauthClientCredentialsCtx.
func (a *MCPAdapter) OAuthClientCredentials() (string, string) {
	return a.oauthClientCredentialsCtx(context.Background())
}

// oauthClientCredentialsCtx reads (client_id, client_secret) from the system
// vault using the caller's context for cancellation/deadlines, preferring an
// admin-pinned override at mcp.oauth.{serviceID} and falling back to the
// dynamically-registered record at mcp.client.{serviceID}.
func (a *MCPAdapter) oauthClientCredentialsCtx(ctx context.Context) (string, string) {
	if a.oauthVault == nil {
		return "", ""
	}
	if cid, csec := adapters.GetMCPOAuthCredentials(ctx, a.oauthVault, a.cfg.ServiceID); cid != "" {
		return cid, csec
	}
	if rec := adapters.GetMCPClientRecord(ctx, a.oauthVault, a.cfg.ServiceID); rec != nil {
		return rec.ClientID, rec.ClientSecret
	}
	return "", ""
}

// EnsureOAuthReady performs discovery + dynamic client registration if no
// client record is cached yet. Idempotent — repeated calls are cheap after
// the first success. Called by the OAuth handlers in services.go just
// before they consult OAuthConfig(); admins who pre-pinned credentials at
// mcp.oauth.{serviceID} bypass this entirely.
//
// redirectURI is the OAuth callback URL Clawvisor will use; passed to the
// MCP authorization server's RFC 7591 registration so the issued client_id
// is bound to it.
func (a *MCPAdapter) EnsureOAuthReady(ctx context.Context, redirectURI string) error {
	if a.oauthSpec() == nil {
		return nil // not an OAuth adapter
	}
	if a.oauthVault == nil {
		return fmt.Errorf("%s: no vault wired", a.cfg.ServiceID)
	}

	// Serialize concurrent first-time activations so two clients don't each
	// run RFC 7591 registration and orphan one client at the auth server.
	// The loser waits, then sees the cached record on recheck and returns
	// without re-registering.
	a.registerMu.Lock()
	defer a.registerMu.Unlock()

	// Admin pinned a client; nothing to discover.
	if cid, _ := adapters.GetMCPOAuthCredentials(ctx, a.oauthVault, a.cfg.ServiceID); cid != "" {
		return nil
	}
	// Already discovered + registered (either by us moments ago or by a
	// previous process); reuse.
	if rec := adapters.GetMCPClientRecord(ctx, a.oauthVault, a.cfg.ServiceID); rec != nil {
		return nil
	}
	// Spec must declare the MCP endpoint to know where to probe.
	if a.cfg.Spec == nil || a.cfg.Spec.MCP.Endpoint == "" {
		return fmt.Errorf("%s: spec is missing mcp.endpoint", a.cfg.ServiceID)
	}

	dctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	httpClient := a.discoveryHTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	rec, err := discoverAndRegister(
		dctx,
		httpClient,
		a.cfg.Spec.MCP.Endpoint,
		"Clawvisor",
		redirectURI,
	)
	if err != nil {
		return fmt.Errorf("%s: discover+register: %w", a.cfg.ServiceID, err)
	}
	if err := adapters.SetMCPClientRecord(ctx, a.oauthVault, a.cfg.ServiceID, rec); err != nil {
		return fmt.Errorf("%s: cache client record: %w", a.cfg.ServiceID, err)
	}
	return nil
}

// SetDiscoveryHTTPClient swaps the *http.Client used for discovery probes
// and registration POSTs. Tests use this to point at httptest.Server.
func (a *MCPAdapter) SetDiscoveryHTTPClient(c *http.Client) {
	a.discoveryHTTPClient = c
}

// oauthConfig is the context-less variant for callers that satisfy the
// adapters.Adapter interface (OAuthConfig() *oauth2.Config). Internal
// callers with a context should use oauthConfigCtx.
func (a *MCPAdapter) oauthConfig() *oauth2.Config {
	return a.oauthConfigCtx(context.Background())
}

// oauthConfigCtx builds the *oauth2.Config that the activation handler in
// services.go uses to drive the authorize → callback → token-exchange flow.
// Returns nil when this isn't an OAuth adapter or when client credentials
// haven't been configured yet (matches the YAMLAdapter convention).
func (a *MCPAdapter) oauthConfigCtx(ctx context.Context) *oauth2.Config {
	spec := a.oauthSpec()
	if spec == nil {
		return nil
	}
	clientID, clientSecret := a.oauthClientCredentialsCtx(ctx)
	if clientID == "" {
		return nil
	}
	// Prefer endpoints discovered via RFC 8414 (cached in the vault under
	// mcp.client.{serviceID}) over anything hardcoded in the spec. Spec
	// values are an explicit override path for MCP servers that don't
	// support discovery — almost no production MCP server should need them.
	authURL := spec.AuthorizeURL
	tokenURL := spec.TokenURL
	if a.oauthVault != nil {
		if rec := adapters.GetMCPClientRecord(ctx, a.oauthVault, a.cfg.ServiceID); rec != nil {
			if rec.AuthorizationEndpoint != "" {
				authURL = rec.AuthorizationEndpoint
			}
			if rec.TokenEndpoint != "" {
				tokenURL = rec.TokenEndpoint
			}
		}
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       spec.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  authURL,
			TokenURL: tokenURL,
		},
	}
}

// httpClientFor returns the *http.Client that the HTTP transport should use
// for one request. For OAuth-MCP it's an oauth2-wrapped client driven by a
// TokenSource — refresh happens transparently on expiry. For static-bearer
// MCP this returns nil and the transport falls back to its default client.
func (a *MCPAdapter) httpClientFor(ctx context.Context, cred []byte) (*http.Client, error) {
	oauthCfg := a.oauthConfigCtx(ctx)
	if oauthCfg == nil {
		return nil, nil
	}
	var stored struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
	}
	if err := json.Unmarshal(cred, &stored); err != nil {
		return nil, fmt.Errorf("parse oauth credential: %w", err)
	}
	if stored.AccessToken == "" && stored.RefreshToken == "" {
		return nil, fmt.Errorf("oauth credential has no tokens")
	}
	token := &oauth2.Token{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		TokenType:    "Bearer",
	}
	if stored.Expiry != "" {
		t, err := time.Parse(time.RFC3339, stored.Expiry)
		if err != nil {
			// A malformed expiry left zero-valued would tell oauth2 the token
			// never expires, suppressing refresh and producing 401s once the
			// access token actually expires. Force a refresh instead by
			// marking it expired in the past.
			token.Expiry = time.Unix(0, 0)
		} else {
			token.Expiry = t
		}
	}
	ts := oauthCfg.TokenSource(ctx, token)
	return oauth2.NewClient(ctx, ts), nil
}
