package llmproxy

import (
	"bytes"
	"context"
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

	var seenAuth, seenAPIKey, seenVersion string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("x-api-key")
		seenVersion = r.Header.Get("anthropic-version")
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

	inbound := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"claude"}`)))
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
	if string(seenBody) != `{"model":"claude"}` {
		t.Errorf("body mismatch: %q", string(seenBody))
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
	if VaultServiceID("unknown") != "" {
		t.Errorf("unknown provider should map to empty serviceID")
	}
}
