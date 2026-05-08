// Package llmproxy implements the lite-proxy LLM endpoint pipeline: it
// terminates Anthropic/OpenAI-compatible requests authenticated by the
// agent's existing token, swaps in the real upstream API key from the
// vault, and streams the response back. Tool-use inspection and resolver
// are layered on top of this in sibling files.
package llmproxy

import (
	"bytes"
	"context"
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

// Forward fetches the upstream API key for (userID, agentID, provider),
// builds an upstream request mirroring the inbound one, injects the
// upstream auth header per-provider, and dispatches via Client. The
// returned *http.Response is the raw upstream response; the caller
// streams its body to the harness.
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

	upstreamURL, err := f.Upstream.URL(provider, inbound.URL.RequestURI())
	if err != nil {
		return nil, err
	}

	keyBytes, serviceID, err := f.lookupVaultKey(ctx, userID, agentID, provider)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(keyBytes)
	_ = serviceID // serviceID is recorded by the handler for audit; future hook

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

// forwardSkipHeaders are stripped from the inbound request when copying
// to the upstream. Most are hop-by-hop or specific to the lite-proxy edge
// (the agent's own bearer/x-api-key carries cvis_… and would 401 upstream;
// the upstream gets its own per-provider header set in injectUpstreamAuth).
//
// All `X-Clawvisor-*` headers are also stripped via prefix match in
// copyForwardableHeaders — they're for the proxy's edge, not the upstream.
var forwardSkipHeaders = map[string]struct{}{
	"authorization":       {}, // agent token is for us, not upstream
	"x-api-key":           {}, // agent token (Anthropic SDK convention) is for us
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
	for name, values := range src {
		lower := strings.ToLower(name)
		if _, skip := forwardSkipHeaders[lower]; skip {
			continue
		}
		if strings.HasPrefix(lower, "x-clawvisor-") {
			continue
		}
		dst[name] = append(dst[name][:0:0], values...)
	}
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
