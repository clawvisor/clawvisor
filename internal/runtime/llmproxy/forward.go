// Package llmproxy implements the lite-proxy LLM endpoint pipeline: it
// terminates Anthropic/OpenAI-compatible requests authenticated by the
// agent's existing token, swaps in the real upstream API key from the vault
// or preserves upstream OAuth in passthrough mode, and streams the response
// back. Tool-use inspection and resolver are layered on top of this in sibling
// files.
package llmproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// DefaultUpstream is the production routing for unmodified deployments.
var DefaultUpstream = UpstreamSelector{
	AnthropicBaseURL: "https://api.anthropic.com",
	OpenAIBaseURL:    "https://api.openai.com",
}

// UpstreamSelector resolves a (provider, path) pair to a concrete upstream
// URL. Configurable to point staging deployments at non-prod hosts and to
// support BYO Bedrock/Vertex/Azure endpoints in future phases.
type UpstreamSelector struct {
	AnthropicBaseURL string
	OpenAIBaseURL    string
}

// URL returns the upstream URL the lite-proxy should forward to for a given
// provider + path.
func (s UpstreamSelector) URL(provider conversation.Provider, path string) (*url.URL, error) {
	switch provider {
	case conversation.ProviderAnthropic:
		return joinURL(s.AnthropicBaseURL, path)
	case conversation.ProviderOpenAI:
		return joinURL(s.OpenAIBaseURL, path)
	}
	return nil, fmt.Errorf("llmproxy: unknown provider %q", provider)
}

func joinURL(base, path string) (*url.URL, error) {
	if base == "" {
		return nil, errors.New("llmproxy: upstream base URL not configured")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("llmproxy: parsing upstream base %q: %w", base, err)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u, nil
}

// VaultServiceID returns the conventional vault service ID under which the
// real upstream API key is stored for a given provider, at user scope.
// The key is fetched via vault.Get(userID, VaultServiceID(provider)).
func VaultServiceID(provider conversation.Provider) string {
	switch provider {
	case conversation.ProviderAnthropic:
		return "anthropic"
	case conversation.ProviderOpenAI:
		return "openai"
	}
	return ""
}

// AgentScopedVaultServiceID returns the vault service ID for a key bound
// to a specific agent. The forwarder tries this first, then falls back
// to the user-scoped key. Format: "agent:<id>:<provider>".
//
// Use case: different agents authenticated by the same user can hit
// different upstream provider keys (different OpenAI orgs, different
// rate-limit tiers, separate billing scopes).
func AgentScopedVaultServiceID(agentID string, provider conversation.Provider) string {
	base := VaultServiceID(provider)
	if base == "" || agentID == "" {
		return ""
	}
	return "agent:" + agentID + ":" + base
}

// Forwarder forwards lite-proxy requests to the real upstream after fetching
// the API key from the vault. It owns no streaming or rewrite logic — that's
// the inspector's job. The returned response is the raw upstream response;
// callers are responsible for closing its body.
type Forwarder struct {
	Vault    vault.Vault
	Client   *http.Client
	Upstream UpstreamSelector
}

// NewForwarder builds a Forwarder with sensible production defaults. The
// http.Client has no overall timeout (SSE streams can be long-lived) but
// the transport caps the time waiting for the response headers — a slow
// or unresponsive upstream can't hold a goroutine forever.
func NewForwarder(v vault.Vault) *Forwarder {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &Forwarder{
		Vault:    v,
		Client:   &http.Client{Timeout: 0, Transport: transport},
		Upstream: DefaultUpstream,
	}
}

// Forward builds an upstream request mirroring the inbound one, injects the
// upstream auth header per-provider or preserves upstream OAuth in passthrough
// mode, and dispatches via Client. The returned *http.Response is the raw
// upstream response; the caller streams its body to the harness.
//
// Vault key resolution order:
//  1. agent-scoped: vault.Get(userID, "agent:<agentID>:<provider>")
//  2. user-scoped:  vault.Get(userID, "<provider>")
//
// Pass an empty agentID to skip the agent-scoped lookup. ErrNotFound on
// agent-scoped is silent (we fall through); ErrNotFound on user-scoped
// is wrapped and returned so the handler can surface UPSTREAM_KEY_MISSING.
func (f *Forwarder) Forward(ctx context.Context, userID, agentID string, provider conversation.Provider, inbound *http.Request, body []byte) (*http.Response, error) {
	if f == nil {
		return nil, errors.New("llmproxy: forwarder is nil")
	}
	if userID == "" {
		return nil, errors.New("llmproxy: userID is empty")
	}
	if inbound == nil || inbound.URL == nil {
		return nil, errors.New("llmproxy: inbound request is nil")
	}

	upstreamURL, err := f.Upstream.URL(provider, inbound.URL.Path)
	if err != nil {
		return nil, err
	}
	// In passthrough mode, OpenAI splits its Codex surface across two
	// upstreams: ChatGPT-OAuth bearers must hit chatgpt.com/backend-api/codex/*
	// while API keys (and OAuth bearers explicitly scoped with
	// api.responses.write) hit api.openai.com/v1/*. Route based on the
	// bearer's shape and claims; fall back to api.openai.com when in doubt.
	if PassthroughUpstreamAuth(ctx) && provider == conversation.ProviderOpenAI {
		if auth := passthroughBearerAuthorization(inbound); auth != "" {
			if routed := openaiPassthroughRoute(auth, inbound.URL.Path); routed != nil {
				upstreamURL = routed
			}
		}
	}
	upstreamURL.RawQuery = inbound.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, inbound.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llmproxy: build upstream request: %w", err)
	}
	copyForwardableHeaders(req.Header, inbound.Header)
	req.Header.Set("Host", upstreamURL.Host)
	req.Host = upstreamURL.Host
	// Force identity encoding upstream. The response postprocess layer
	// parses the body for tool_use rewriting; gzip would silently disable
	// rewrites by making the body unparseable. Cost: marginally larger
	// SSE payload from upstream.
	req.Header.Set("Accept-Encoding", "identity")

	if PassthroughUpstreamAuth(ctx) {
		if auth := passthroughBearerAuthorization(inbound); auth != "" {
			if provider == conversation.ProviderOpenAI {
				injectPassthroughOpenAIAuth(req, auth)
				return f.Client.Do(req)
			}
			if provider == conversation.ProviderAnthropic {
				injectPassthroughAnthropicAuth(req, auth)
				return f.Client.Do(req)
			}
		}
	}

	keyBytes, serviceID, err := f.lookupVaultKey(ctx, userID, agentID, provider)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(keyBytes)
	_ = serviceID // serviceID is recorded by the handler for audit; future hook

	if err := injectUpstreamAuth(req, provider, keyBytes); err != nil {
		return nil, err
	}

	return f.Client.Do(req)
}

// lookupVaultKey resolves the upstream API key with agent-scoped-first
// fallback to user-scoped. Returns (key bytes, the serviceID actually
// used, error). The serviceID is useful for audit so the row records
// whether the agent-scoped or user-scoped key was used.
func (f *Forwarder) lookupVaultKey(ctx context.Context, userID, agentID string, provider conversation.Provider) ([]byte, string, error) {
	if agentID != "" {
		if scoped := AgentScopedVaultServiceID(agentID, provider); scoped != "" {
			key, err := f.Vault.Get(ctx, userID, scoped)
			if err == nil {
				return key, scoped, nil
			}
			if !errors.Is(err, vault.ErrNotFound) {
				return nil, "", fmt.Errorf("llmproxy: vault get agent-scoped: %w", err)
			}
			// Fall through to user-scoped.
		}
	}
	userServiceID := VaultServiceID(provider)
	if userServiceID == "" {
		return nil, "", fmt.Errorf("llmproxy: no vault service ID for provider %q", provider)
	}
	key, err := f.Vault.Get(ctx, userID, userServiceID)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			return nil, userServiceID, fmt.Errorf("llmproxy: upstream credential not found in vault for service %q: %w", userServiceID, vault.ErrNotFound)
		}
		return nil, userServiceID, fmt.Errorf("llmproxy: vault get: %w", err)
	}
	return key, userServiceID, nil
}

// injectUpstreamAuth writes the upstream-specific auth header using the raw
// API key bytes. Handles both Anthropic (x-api-key + anthropic-version) and
// OpenAI (Authorization: Bearer).
func injectUpstreamAuth(req *http.Request, provider conversation.Provider, key []byte) error {
	keyStr := strings.TrimSpace(string(key))
	switch provider {
	case conversation.ProviderAnthropic:
		req.Header.Set("x-api-key", keyStr)
		req.Header.Del("Authorization") // strip caller auth so we don't double-up
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case conversation.ProviderOpenAI:
		req.Header.Set("Authorization", "Bearer "+keyStr)
	default:
		return fmt.Errorf("llmproxy: unknown provider %q", provider)
	}
	return nil
}

func passthroughBearerAuthorization(inbound *http.Request) string {
	if inbound == nil {
		return ""
	}
	auth := strings.TrimSpace(inbound.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	token := strings.TrimSpace(auth[len(prefix):])
	if token == "" || strings.HasPrefix(token, "cvis_") {
		return ""
	}
	return prefix + token
}

// openaiPassthroughRoute decides which OpenAI upstream a passthrough request
// should hit. ChatGPT-OAuth JWT bearers — JWTs whose scope claims don't
// include api.responses.write — must be routed to
// chatgpt.com/backend-api/codex/*; everything else (sk-* / sk-proj-* API
// keys, JWTs with api.responses.write, unrecognized tokens) falls back to
// api.openai.com/v1/*. Returns nil to mean "use default routing."
//
// The Authorization header argument is the full "Bearer <token>" string.
// The path argument is the inbound request path (typically /v1/responses).
func openaiPassthroughRoute(authorization, inboundPath string) *url.URL {
	const prefix = "Bearer "
	token := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	if token == "" {
		return nil
	}
	// API keys: sk-* and sk-proj-* go to the API endpoint.
	if strings.HasPrefix(token, "sk-") {
		return nil
	}
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some JWT emitters use padded base64; tolerate that.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	// Codex's access_token has scp as a JSON array; other OAuth providers
	// emit space-separated strings. Decode both into json.RawMessage and
	// normalize.
	var claims struct {
		Scp   json.RawMessage `json:"scp"`
		Scope json.RawMessage `json:"scope"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	if scopesIncludes(claims.Scp, "api.responses.write") || scopesIncludes(claims.Scope, "api.responses.write") {
		return nil
	}
	// ChatGPT-OAuth JWT without api.responses.write: route to chatgpt.com.
	// Path mapping: /v1/<rest> → /backend-api/codex/<rest>.
	suffix := strings.TrimPrefix(inboundPath, "/v1")
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return &url.URL{
		Scheme: "https",
		Host:   "chatgpt.com",
		Path:   "/backend-api/codex" + suffix,
	}
}

// scopesIncludes checks for a target scope in either of the two shapes OAuth
// providers emit: a space-separated string (RFC 6749 oauth2 standard) or a
// JSON array of strings (what Codex's access_token uses).
func scopesIncludes(raw json.RawMessage, want string) bool {
	if len(raw) == 0 || want == "" {
		return false
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return false
		}
		for _, t := range strings.Fields(s) {
			if t == want {
				return true
			}
		}
		return false
	}
	if raw[0] == '[' {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return false
		}
		for _, t := range list {
			if t == want {
				return true
			}
		}
	}
	return false
}

func injectPassthroughOpenAIAuth(req *http.Request, authorization string) {
	req.Header.Set("Authorization", authorization)
	req.Header.Del("x-api-key")
}

func injectPassthroughAnthropicAuth(req *http.Request, authorization string) {
	req.Header.Set("Authorization", authorization)
	req.Header.Del("x-api-key")
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
}

// forwardSkipHeaders are stripped from the inbound request when copying
// to the upstream. Most are hop-by-hop or specific to the lite-proxy edge
// (the agent's own bearer/x-api-key may carry cvis_… and would 401 upstream;
// upstream auth is restored explicitly after this copy step).
//
// All `X-Clawvisor-*` headers are also stripped via prefix match in
// copyForwardableHeaders — they're for the proxy's edge, not the upstream.
var forwardSkipHeaders = map[string]struct{}{
	"authorization":       {}, // agent token is for us, not upstream
	"x-api-key":           {}, // agent token (Anthropic SDK convention) is for us
	"cookie":              {}, // session cookies for the clawvisor UI must not reach api.anthropic.com / api.openai.com
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"host":                {},
	"content-length":      {}, // http.NewRequest manages content-length itself
}

func copyForwardableHeaders(dst, src http.Header) {
	// Per RFC 7230, headers named in the inbound Connection field are
	// hop-by-hop and must not be forwarded. Static denylists alone miss
	// non-standard connection-scoped headers like X-Proxy-Internal.
	connectionDrop := connectionScopedHeaders(src)
	for name, values := range src {
		lower := strings.ToLower(name)
		if _, skip := forwardSkipHeaders[lower]; skip {
			continue
		}
		if _, skip := connectionDrop[lower]; skip {
			continue
		}
		if strings.HasPrefix(lower, "x-clawvisor-") {
			continue
		}
		dst[name] = append(dst[name][:0:0], values...)
	}
}

// connectionScopedHeaders parses the inbound Connection header(s) and
// returns the lowercased header names that should be dropped along with
// Connection itself. The set is empty when no Connection header is set.
func connectionScopedHeaders(src http.Header) map[string]struct{} {
	values := src.Values("Connection")
	if len(values) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, v := range values {
		for _, token := range strings.Split(v, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token == "" || token == "close" || token == "keep-alive" {
				continue
			}
			out[token] = struct{}{}
		}
	}
	return out
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
