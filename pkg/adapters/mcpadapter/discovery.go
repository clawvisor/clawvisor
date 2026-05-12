package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// ResourceMetadata is the RFC 9728 "OAuth 2.0 Protected Resource Metadata"
// response that the MCP endpoint points to via WWW-Authenticate. We only
// pull the field we need: the list of authorization servers.
type resourceMetadata struct {
	AuthorizationServers []string `json:"authorization_servers"`
}

// AuthServerMetadata is the RFC 8414 "OAuth 2.0 Authorization Server Metadata"
// response. Subset of fields the adapter actually uses.
type authServerMetadata struct {
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	GrantTypesSupported   []string `json:"grant_types_supported"`
	AuthMethodsSupported  []string `json:"token_endpoint_auth_methods_supported"`
}

// resourceMetadataRe extracts the resource_metadata URL from a
// WWW-Authenticate header.  Notion's header looks like:
//
//	Bearer realm="OAuth", resource_metadata="https://mcp.notion.com/.well-known/oauth-protected-resource/mcp", error="invalid_token", error_description="..."
var resourceMetadataRe = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// discoverAndRegister performs the full RFC 9728 → RFC 8414 → RFC 7591
// dance against an MCP server's endpoint. Returns a populated client
// record ready to be cached + used for OAuth.
//
// httpClient is used for all probes/registrations. Caller supplies it so
// tests can swap in httptest-bound transports without DNS.
func discoverAndRegister(
	ctx context.Context,
	httpClient *http.Client,
	mcpEndpoint string,
	clientName string,
	redirectURI string,
) (*adapters.MCPClientRecord, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	// ── Step 1: probe the MCP endpoint for the WWW-Authenticate header. ──
	probeBody := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpEndpoint, bytes.NewReader(probeBody))
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", mcpEndpoint, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	wwwAuth := resp.Header.Get("Www-Authenticate")
	if wwwAuth == "" {
		// Some servers may advertise discovery at a fixed well-known URL
		// off the endpoint root even without a 401 probe. Try that as a
		// fallback before giving up.
		wwwAuth = `resource_metadata="` + wellKnownProtectedResourceFor(mcpEndpoint) + `"`
	}
	m := resourceMetadataRe.FindStringSubmatch(wwwAuth)
	if len(m) < 2 {
		return nil, fmt.Errorf("server did not advertise resource_metadata; got WWW-Authenticate=%q", wwwAuth)
	}
	resourceMetaURL := m[1]

	// ── Step 2: fetch protected-resource metadata. ──
	var resMeta resourceMetadata
	if err := getJSON(ctx, httpClient, resourceMetaURL, &resMeta); err != nil {
		return nil, fmt.Errorf("fetch protected-resource metadata: %w", err)
	}
	if len(resMeta.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("protected-resource metadata at %s has no authorization_servers", resourceMetaURL)
	}
	authServer := strings.TrimRight(resMeta.AuthorizationServers[0], "/")

	// ── Step 3: fetch authorization-server metadata. ──
	asMetaURL := authServer + "/.well-known/oauth-authorization-server"
	var asMeta authServerMetadata
	if err := getJSON(ctx, httpClient, asMetaURL, &asMeta); err != nil {
		return nil, fmt.Errorf("fetch authorization-server metadata: %w", err)
	}
	if asMeta.AuthorizationEndpoint == "" || asMeta.TokenEndpoint == "" {
		return nil, fmt.Errorf("authorization-server metadata at %s missing endpoints", asMetaURL)
	}
	if asMeta.RegistrationEndpoint == "" {
		return nil, fmt.Errorf("authorization-server at %s does not support RFC 7591 dynamic registration; admin must pre-register a client", authServer)
	}

	// ── Step 4: dynamically register a client. ──
	// Prefer a confidential client (with secret) so the existing OAuth code
	// path in services.go works unchanged. Public clients (token_endpoint_
	// auth_method=none) require PKCE on the token exchange, which the
	// standard OAuth flow at services.go doesn't currently add — so refuse
	// to register a client we can't use, rather than registering one that
	// would fail at token exchange or silently bypass auth-method
	// expectations on the server.
	// RFC 8414 §2: when token_endpoint_auth_methods_supported is absent the
	// default is ["client_secret_basic"]. Treat nil/empty as that default so
	// minimally-compliant servers don't fail discovery.
	supported := asMeta.AuthMethodsSupported
	if len(supported) == 0 {
		supported = []string{"client_secret_basic"}
	}
	authMethod := "client_secret_post"
	switch {
	case supportsAuthMethod(supported, "client_secret_post"):
		authMethod = "client_secret_post"
	case supportsAuthMethod(supported, "client_secret_basic"):
		authMethod = "client_secret_basic"
	default:
		return nil, fmt.Errorf("authorization server only supports public OAuth clients (token_endpoint_auth_methods_supported=%v); Clawvisor doesn't yet implement PKCE for MCP — admin must pre-register a confidential client and pin it via settings", supported)
	}

	regReq := map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": authMethod,
		"application_type":           "web",
	}
	body, _ := json.Marshal(regReq)
	rreq, err := http.NewRequestWithContext(ctx, http.MethodPost, asMeta.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	rreq.Header.Set("Content-Type", "application/json")
	rreq.Header.Set("Accept", "application/json")
	rresp, err := httpClient.Do(rreq)
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", asMeta.RegistrationEndpoint, err)
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusCreated && rresp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(rresp.Body, 2048))
		return nil, fmt.Errorf("register: http %d: %s", rresp.StatusCode, string(raw))
	}
	var regResp struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(rresp.Body).Decode(&regResp); err != nil {
		return nil, fmt.Errorf("register: decode: %w", err)
	}
	if regResp.ClientID == "" {
		return nil, fmt.Errorf("register: server returned no client_id")
	}

	return &adapters.MCPClientRecord{
		ClientID:              regResp.ClientID,
		ClientSecret:          regResp.ClientSecret,
		AuthorizationEndpoint: asMeta.AuthorizationEndpoint,
		TokenEndpoint:         asMeta.TokenEndpoint,
		RegistrationEndpoint:  asMeta.RegistrationEndpoint,
	}, nil
}

// getJSON does a context-bound GET and decodes JSON into out.
func getJSON(ctx context.Context, httpClient *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// wellKnownProtectedResourceFor builds the conventional discovery URL
// derived from the MCP endpoint root, used as a fallback when the server
// doesn't return a WWW-Authenticate header on probe.
//
// Parses with net/url so http:// endpoints, custom ports, IPv6 hosts, and
// edge-cases like missing schemes don't panic or mis-slice.
func wellKnownProtectedResourceFor(mcpEndpoint string) string {
	u, err := url.Parse(mcpEndpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Best-effort fallback for malformed inputs: append the well-known
		// path to whatever we got. Discovery will fail downstream and the
		// caller surfaces the error to the OAuth handler.
		return strings.TrimRight(mcpEndpoint, "/") + "/.well-known/oauth-protected-resource"
	}
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
}

func supportsAuthMethod(supported []string, method string) bool {
	for _, m := range supported {
		if m == method {
			return true
		}
	}
	return false
}

// discoveryTimeout caps the discovery+registration round trip. Cold-start
// against Notion's MCP runs ~600ms; 10s is generous headroom.
const discoveryTimeout = 10 * time.Second
