package llmproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type stubVault struct {
	stored map[string][]byte
	err    error
}

func (s *stubVault) Set(ctx context.Context, userID, serviceID string, credential []byte) error {
	if s.stored == nil {
		s.stored = map[string][]byte{}
	}
	s.stored[userID+"/"+serviceID] = append([]byte{}, credential...)
	return nil
}

func (s *stubVault) SetIfAbsent(ctx context.Context, userID, serviceID string, credential []byte) error {
	if s.stored == nil {
		s.stored = map[string][]byte{}
	}
	if _, ok := s.stored[userID+"/"+serviceID]; ok {
		return vault.ErrAlreadyExists
	}
	s.stored[userID+"/"+serviceID] = append([]byte{}, credential...)
	return nil
}

func (s *stubVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.stored[userID+"/"+serviceID]
	if !ok {
		return nil, vault.ErrNotFound
	}
	return append([]byte{}, v...), nil
}

func (s *stubVault) Delete(ctx context.Context, userID, serviceID string) error {
	delete(s.stored, userID+"/"+serviceID)
	return nil
}

func (s *stubVault) List(ctx context.Context, userID string) ([]string, error) { return nil, nil }

func TestForward_AnthropicInjectsKey(t *testing.T) {
	v := &stubVault{}
	v.Set(context.Background(), "user1", "anthropic", []byte("sk-ant-real-key"))

	var seenAuth, seenAPIKey, seenVersion, seenPath, seenQuery string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("x-api-key")
		seenVersion = r.Header.Get("anthropic-version")
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{
		AnthropicBaseURL: upstream.URL,
		OpenAIBaseURL:    "",
	}

	inbound := httptest.NewRequest(http.MethodPost, "/api/v1/messages?beta=true", bytes.NewReader([]byte(`{"model":"claude"}`)))
	inbound.Header.Set("Authorization", "Bearer cvis_xxx")
	inbound.Header.Set("anthropic-beta", "beta1")

	resp, err := f.Forward(context.Background(), "user1", "", conversation.ProviderAnthropic, inbound, []byte(`{"model":"claude"}`))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAuth != "" {
		t.Errorf("expected upstream Authorization to be stripped, got %q", seenAuth)
	}
	if seenAPIKey != "sk-ant-real-key" {
		t.Errorf("expected x-api-key=sk-ant-real-key, got %q", seenAPIKey)
	}
	if seenVersion == "" {
		t.Errorf("expected default anthropic-version header")
	}
	if seenPath != "/v1/messages" || seenQuery != "beta=true" {
		t.Errorf("expected upstream path/query /v1/messages?beta=true, got %q?%q", seenPath, seenQuery)
	}
	if string(seenBody) != `{"model":"claude"}` {
		t.Errorf("body mismatch: %q", string(seenBody))
	}
}

func TestForward_AnthropicPassthroughAuthPreservesOAuthAuthorization(t *testing.T) {
	v := &stubVault{}

	var seenAuth, seenAPIKey, seenGoogleAPIKey, seenVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("x-api-key")
		seenGoogleAPIKey = r.Header.Get("x-goog-api-key")
		seenVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude"}`)))
	inbound.Header.Set("Authorization", "Bearer claude-oauth-token")
	inbound.Header.Set("x-api-key", "cvis_agent_token")
	inbound.Header.Set("x-goog-api-key", "caller-google-key")

	ctx := WithPassthroughUpstreamAuth(context.Background())
	resp, err := f.Forward(ctx, "user1", "", conversation.ProviderAnthropic, inbound, []byte(`{"model":"claude"}`))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAuth != "Bearer claude-oauth-token" {
		t.Fatalf("expected upstream OAuth Authorization to pass through, got %q", seenAuth)
	}
	if seenAPIKey != "" {
		t.Fatalf("expected upstream x-api-key stripped in passthrough mode, got %q", seenAPIKey)
	}
	if seenGoogleAPIKey != "" {
		t.Fatalf("expected upstream x-goog-api-key stripped in passthrough mode, got %q", seenGoogleAPIKey)
	}
	if seenVersion == "" {
		t.Errorf("expected default anthropic-version header")
	}
}

func TestForward_OpenAIInjectsKey(t *testing.T) {
	v := &stubVault{}
	v.Set(context.Background(), "user1", "openai", []byte("sk-real-openai-key"))

	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{OpenAIBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	resp, err := f.Forward(context.Background(), "user1", "", conversation.ProviderOpenAI, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAuth != "Bearer sk-real-openai-key" {
		t.Errorf("expected Bearer sk-real-openai-key, got %q", seenAuth)
	}
}

func TestForward_OpenAIPassthroughAuthPreservesOAuthAuthorization(t *testing.T) {
	v := &stubVault{}

	var seenAuth, seenAccountID, seenGoogleAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAccountID = r.Header.Get("ChatGPT-Account-Id")
		seenGoogleAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{OpenAIBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	inbound.Header.Set("Authorization", "Bearer codex-oauth-token")
	inbound.Header.Set("ChatGPT-Account-Id", "acct_123")
	inbound.Header.Set("X-Clawvisor-Agent-Token", "cvis_agent_token")
	inbound.Header.Set("x-goog-api-key", "caller-google-key")

	ctx := WithPassthroughUpstreamAuth(context.Background())
	resp, err := f.Forward(ctx, "user1", "", conversation.ProviderOpenAI, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAuth != "Bearer codex-oauth-token" {
		t.Fatalf("expected upstream OAuth Authorization to pass through, got %q", seenAuth)
	}
	if seenAccountID != "acct_123" {
		t.Fatalf("expected ChatGPT-Account-Id to pass through when present, got %q", seenAccountID)
	}
	if seenGoogleAPIKey != "" {
		t.Fatalf("expected upstream x-goog-api-key stripped in passthrough mode, got %q", seenGoogleAPIKey)
	}
}

func TestForward_GoogleInjectsKey(t *testing.T) {
	v := &stubVault{}
	v.Set(context.Background(), "user1", "google", []byte("gemini-real-key"))

	var seenAuth, seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{GoogleBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:generateContent", strings.NewReader("{}"))
	inbound.Header.Set("Authorization", "Bearer cvis_agent_token")

	resp, err := f.Forward(context.Background(), "user1", "", conversation.ProviderGoogle, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAPIKey != "gemini-real-key" {
		t.Fatalf("expected x-goog-api-key=gemini-real-key, got %q", seenAPIKey)
	}
	if seenAuth != "" {
		t.Fatalf("expected caller Authorization stripped for Google key auth, got %q", seenAuth)
	}
}

func TestForward_GooglePassthroughAuthPreservesOAuthAuthorization(t *testing.T) {
	v := &stubVault{}

	var seenAuth, seenAPIKey, seenXAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("x-goog-api-key")
		seenXAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{GoogleBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:generateContent", strings.NewReader("{}"))
	inbound.Header.Set("Authorization", "Bearer google-oauth-token")
	inbound.Header.Set("x-goog-api-key", "cvis_agent_token")
	inbound.Header.Set("x-api-key", "caller-anthropic-key")

	ctx := WithPassthroughUpstreamAuth(context.Background())
	resp, err := f.Forward(ctx, "user1", "", conversation.ProviderGoogle, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAuth != "Bearer google-oauth-token" {
		t.Fatalf("expected upstream OAuth Authorization to pass through, got %q", seenAuth)
	}
	if seenAPIKey != "" {
		t.Fatalf("expected upstream x-goog-api-key stripped in passthrough mode, got %q", seenAPIKey)
	}
	if seenXAPIKey != "" {
		t.Fatalf("expected upstream x-api-key stripped in passthrough mode, got %q", seenXAPIKey)
	}
}

func TestForward_VaultMissing(t *testing.T) {
	v := &stubVault{}
	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: "http://localhost"}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	_, err := f.Forward(context.Background(), "user1", "", conversation.ProviderAnthropic, inbound, []byte("{}"))
	if err == nil {
		t.Fatalf("expected error on missing vault key")
	}
}

func TestUpstreamSelector_URL(t *testing.T) {
	s := UpstreamSelector{
		AnthropicBaseURL: "https://api.anthropic.com",
		OpenAIBaseURL:    "https://api.openai.com",
	}
	u, err := s.URL("anthropic", "/v1/messages")
	if err != nil || u.String() != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("anthropic URL: %v %v", u, err)
	}
	u, err = s.URL("openai", "/v1/chat/completions")
	if err != nil || u.String() != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("openai URL: %v %v", u, err)
	}
	if _, err := s.URL("unknown", "/v1/x"); err == nil {
		t.Fatalf("expected error on unknown provider")
	}
}

func TestForward_ForcesIdentityEncoding(t *testing.T) {
	v := &stubVault{}
	v.Set(context.Background(), "user1", "anthropic", []byte("sk-ant-real-key"))

	var seenAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAcceptEncoding = r.Header.Get("Accept-Encoding")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	// Harness asks for gzip — forwarder should override with identity.
	inbound.Header.Set("Accept-Encoding", "gzip, deflate, br")

	resp, err := f.Forward(context.Background(), "user1", "", conversation.ProviderAnthropic, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenAcceptEncoding != "identity" {
		t.Errorf("expected upstream Accept-Encoding=identity, got %q", seenAcceptEncoding)
	}
}

func TestForward_StripsXClawvisorPrefix(t *testing.T) {
	v := &stubVault{}
	v.Set(context.Background(), "user1", "anthropic", []byte("sk-ant-real-key"))

	var seenHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	inbound.Header.Set("X-Clawvisor-Caller", "Bearer cvis_x")
	inbound.Header.Set("X-Clawvisor-Custom", "leaked?")
	inbound.Header.Set("X-Clawvisor-session", "abc")

	resp, err := f.Forward(context.Background(), "user1", "", conversation.ProviderAnthropic, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	for name := range seenHeaders {
		if strings.HasPrefix(strings.ToLower(name), "x-clawvisor-") {
			t.Fatalf("X-Clawvisor-* leaked to upstream: %s", name)
		}
	}
}

// Per-agent vault keys: forwarder tries agent-scoped first, falls back
// to user-scoped when the agent-specific key is absent.
func TestForward_AgentScopedKeyTakesPrecedence(t *testing.T) {
	v := &stubVault{}
	// Both keys in vault — agent-scoped should win.
	v.Set(context.Background(), "user1", "anthropic", []byte("sk-ant-USER-fallback"))
	v.Set(context.Background(), "user1", "agent:agentA:anthropic", []byte("sk-ant-AGENT-A-key"))

	var seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := f.Forward(context.Background(), "user1", "agentA", conversation.ProviderAnthropic, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if seenAPIKey != "sk-ant-AGENT-A-key" {
		t.Errorf("agent-scoped key should win; got %q", seenAPIKey)
	}
}

func TestForward_FallsBackToUserKeyWhenAgentKeyAbsent(t *testing.T) {
	v := &stubVault{}
	v.Set(context.Background(), "user1", "anthropic", []byte("sk-ant-USER-fallback"))
	// Note: NO agent-scoped key.

	var seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := f.Forward(context.Background(), "user1", "agentA", conversation.ProviderAnthropic, inbound, []byte("{}"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if seenAPIKey != "sk-ant-USER-fallback" {
		t.Errorf("user-scoped key should be the fallback; got %q", seenAPIKey)
	}
}

func TestAgentScopedVaultServiceID(t *testing.T) {
	if got := AgentScopedVaultServiceID("agentA", conversation.ProviderAnthropic); got != "agent:agentA:anthropic" {
		t.Errorf("got %q, want agent:agentA:anthropic", got)
	}
	if got := AgentScopedVaultServiceID("", conversation.ProviderAnthropic); got != "" {
		t.Errorf("empty agentID should return empty; got %q", got)
	}
}

func TestVaultServiceID(t *testing.T) {
	if VaultServiceID(conversation.ProviderAnthropic) != "anthropic" {
		t.Errorf("anthropic service id mismatch")
	}
	if VaultServiceID(conversation.ProviderOpenAI) != "openai" {
		t.Errorf("openai service id mismatch")
	}
	if VaultServiceID(conversation.ProviderGoogle) != "google" {
		t.Errorf("google service id mismatch")
	}
	if VaultServiceID("unknown") != "" {
		t.Errorf("unknown provider should map to empty serviceID")
	}
}

// RFC 7230: headers named in the inbound Connection field are
// hop-by-hop and must not be forwarded.
func TestCopyForwardableHeaders_StripsConnectionScoped(t *testing.T) {
	src := http.Header{}
	src.Set("Connection", "X-Internal, Upgrade")
	src.Set("X-Internal", "secret-internal-token")
	src.Set("Upgrade", "websocket")
	src.Set("X-Forwarded-For", "1.2.3.4")
	src.Set("Authorization", "Bearer cvis_agent_token")
	src.Set("x-goog-api-key", "caller-google-key")
	dst := http.Header{}
	copyForwardableHeaders(dst, src)
	if dst.Get("X-Internal") != "" {
		t.Errorf("X-Internal listed in Connection should be stripped, got %q", dst.Get("X-Internal"))
	}
	if dst.Get("Upgrade") != "" {
		t.Errorf("Upgrade should be stripped, got %q", dst.Get("Upgrade"))
	}
	if dst.Get("X-Forwarded-For") != "1.2.3.4" {
		t.Errorf("X-Forwarded-For (not connection-scoped) should pass through, got %q", dst.Get("X-Forwarded-For"))
	}
	if dst.Get("Authorization") != "" {
		t.Errorf("Authorization (static skip) should still be stripped, got %q", dst.Get("Authorization"))
	}
	if dst.Get("x-goog-api-key") != "" {
		t.Errorf("x-goog-api-key (static skip) should be stripped, got %q", dst.Get("x-goog-api-key"))
	}
}

// makeJWT builds a syntactically-valid JWT with the given claims for routing
// tests. Signature is not verified — the routing helper inspects claims only.
func makeJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + body + "." + sig
}

func TestOpenAIPassthroughRoute_APIKeyFallsBackToDefault(t *testing.T) {
	if got := openaiPassthroughRoute("Bearer sk-abc123", "/v1/responses"); got != nil {
		t.Errorf("sk-* API key must use default api.openai.com routing, got %s", got)
	}
	if got := openaiPassthroughRoute("Bearer sk-proj-abc123", "/v1/responses"); got != nil {
		t.Errorf("sk-proj-* API key must use default routing, got %s", got)
	}
}

func TestOpenAIPassthroughRoute_OAuthWithResponseScope(t *testing.T) {
	// Space-separated string form (RFC 6749).
	jwt := makeJWT(map[string]any{"scp": "api.responses.write other.scope"})
	if got := openaiPassthroughRoute("Bearer "+jwt, "/v1/responses"); got != nil {
		t.Errorf("OAuth bearer with api.responses.write scp (string) must use default routing, got %s", got)
	}
	// Array form (Codex's actual token format).
	jwtArr := makeJWT(map[string]any{"scp": []string{"openid", "api.responses.write", "email"}})
	if got := openaiPassthroughRoute("Bearer "+jwtArr, "/v1/responses"); got != nil {
		t.Errorf("OAuth bearer with api.responses.write scp (array) must use default routing, got %s", got)
	}
	jwtScope := makeJWT(map[string]any{"scope": "api.responses.write"})
	if got := openaiPassthroughRoute("Bearer "+jwtScope, "/v1/responses"); got != nil {
		t.Errorf("OAuth bearer with api.responses.write scope must use default routing, got %s", got)
	}
}

// Regression: Codex's access_token has scp as a JSON array. Earlier the
// helper assumed scp was a space-separated string, so unmarshal failed and
// we fell through to default routing (api.openai.com), which rejected the
// bearer for missing api scopes.
func TestOpenAIPassthroughRoute_CodexAccessTokenShape(t *testing.T) {
	jwt := makeJWT(map[string]any{
		"iss": "https://auth.openai.com",
		"scp": []string{"openid", "profile", "email", "offline_access", "api.connectors.read", "api.connectors.invoke"},
	})
	got := openaiPassthroughRoute("Bearer "+jwt, "/v1/responses")
	if got == nil {
		t.Fatal("Codex-shaped access_token (scp as array without api.responses.write) must route to chatgpt.com")
	}
	if got.Host != "chatgpt.com" || got.Path != "/backend-api/codex/responses" {
		t.Errorf("expected https://chatgpt.com/backend-api/codex/responses, got %s://%s%s", got.Scheme, got.Host, got.Path)
	}
}

func TestOpenAIPassthroughRoute_ChatGPTOAuth(t *testing.T) {
	jwt := makeJWT(map[string]any{
		"scp":                "openid email profile",
		"chatgpt_account_id": "acct_xyz",
	})
	got := openaiPassthroughRoute("Bearer "+jwt, "/v1/responses")
	if got == nil {
		t.Fatal("ChatGPT-OAuth bearer (no api.responses.write) must route to chatgpt.com")
	}
	if got.Host != "chatgpt.com" {
		t.Errorf("expected chatgpt.com host, got %s", got.Host)
	}
	if got.Path != "/backend-api/codex/responses" {
		t.Errorf("expected /backend-api/codex/responses, got %s", got.Path)
	}
	if got.Scheme != "https" {
		t.Errorf("expected https scheme, got %s", got.Scheme)
	}
}

// TestOpenAIPassthroughRoute_ChatGPTUpstreamOverride covers the env-var
// override paths for CLAWVISOR_LLM_UPSTREAM_CHATGPT. The protocol-
// relative case is a regression test: url.Parse("//host:port") returns
// scheme="" host="host:port" — without a fallback, that empty scheme
// would leak into the request and trip http.Client.Do at runtime.
func TestOpenAIPassthroughRoute_ChatGPTUpstreamOverride(t *testing.T) {
	jwt := makeJWT(map[string]any{"scp": "openid"})

	tests := []struct {
		name       string
		override   string
		wantScheme string
		wantHost   string
	}{
		{name: "unset", override: "", wantScheme: "https", wantHost: "chatgpt.com"},
		{name: "explicit_http", override: "http://localhost:8080", wantScheme: "http", wantHost: "localhost:8080"},
		{name: "explicit_https", override: "https://staging.chatgpt.example.com", wantScheme: "https", wantHost: "staging.chatgpt.example.com"},
		// Regression: protocol-relative URL has populated Host but
		// empty Scheme. Without the fallback we'd build a URL with no
		// scheme and http.Client.Do would reject it.
		{name: "protocol_relative_defaults_to_https", override: "//localhost:8080", wantScheme: "https", wantHost: "localhost:8080"},
		// Bare hostnames with ports look like a scheme + opaque to
		// url.Parse, so they get rejected by the host!="" guard and
		// fall back to the default. Documenting current behavior.
		{name: "bare_host_port_falls_back", override: "localhost:9000", wantScheme: "https", wantHost: "chatgpt.com"},
		// Garbage URL also falls back.
		{name: "garbage_falls_back", override: ":::not a url", wantScheme: "https", wantHost: "chatgpt.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAWVISOR_LLM_UPSTREAM_CHATGPT", tc.override)
			got := openaiPassthroughRoute("Bearer "+jwt, "/v1/responses")
			if got == nil {
				t.Fatal("ChatGPT-OAuth bearer must route to chatgpt.com (or override)")
			}
			if got.Scheme != tc.wantScheme {
				t.Errorf("scheme=%q, want %q", got.Scheme, tc.wantScheme)
			}
			if got.Host != tc.wantHost {
				t.Errorf("host=%q, want %q", got.Host, tc.wantHost)
			}
			// Whatever scheme/host, the path mapping must still apply.
			if got.Path != "/backend-api/codex/responses" {
				t.Errorf("path=%q, want /backend-api/codex/responses", got.Path)
			}
		})
	}
}

func TestOpenAIPassthroughRoute_MalformedTokenFallsBack(t *testing.T) {
	if got := openaiPassthroughRoute("Bearer not.a.jwt.too.many.dots", "/v1/responses"); got != nil {
		t.Errorf("malformed token must fall back to default routing, got %s", got)
	}
	if got := openaiPassthroughRoute("Bearer notjwt", "/v1/responses"); got != nil {
		t.Errorf("non-JWT non-API-key must fall back, got %s", got)
	}
	if got := openaiPassthroughRoute("", "/v1/responses"); got != nil {
		t.Errorf("empty bearer must return nil, got %s", got)
	}
}

func TestOpenAIPassthroughRoute_NonV1PathPreserved(t *testing.T) {
	jwt := makeJWT(map[string]any{"scp": "openid"})
	got := openaiPassthroughRoute("Bearer "+jwt, "/v1/chat/completions")
	if got == nil || got.Path != "/backend-api/codex/chat/completions" {
		t.Errorf("expected /backend-api/codex/chat/completions, got %v", got)
	}
}
