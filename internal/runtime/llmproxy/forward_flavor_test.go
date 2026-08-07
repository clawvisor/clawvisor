package llmproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/upstreamflavor"
)

// simpleFlavor implements upstreamflavor.Flavor via closures so each
// test can configure the three hooks independently.
type simpleFlavor struct {
	name           string
	vaultServiceID string
	buildURL       func(path, model string) (*url.URL, error)
	transformBody  func([]byte) ([]byte, error)
	injectAuth     func(*http.Request, []byte) error

	// recorded
	gotModel string
}

var _ upstreamflavor.Flavor = (*simpleFlavor)(nil)

func (s *simpleFlavor) Name() string { return s.name }
func (s *simpleFlavor) BuildURL(path, model string) (*url.URL, error) {
	s.gotModel = model
	return s.buildURL(path, model)
}
func (s *simpleFlavor) TransformBody(b []byte) ([]byte, error) { return s.transformBody(b) }
func (s *simpleFlavor) InjectAuth(req *http.Request, c []byte) error {
	return s.injectAuth(req, c)
}
func (s *simpleFlavor) VaultServiceID() string { return s.vaultServiceID }

func TestForward_FlavorDispatch(t *testing.T) {
	t.Parallel()

	var seenAuth, seenAPIKey, seenPath, seenAnthropicVersion string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("api-key")
		seenAnthropicVersion = r.Header.Get("anthropic-version")
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_x"}`))
	}))
	defer upstream.Close()

	flavor := &simpleFlavor{
		name:           "fake",
		vaultServiceID: "fake",
		buildURL: func(path, model string) (*url.URL, error) {
			return url.Parse(upstream.URL + path)
		},
		transformBody: func(b []byte) ([]byte, error) {
			// Strip model + add a sentinel so we can verify the
			// transformed body reached the upstream.
			out := bytes.ReplaceAll(b, []byte(`"model":"claude-opus-4-7",`), nil)
			out = bytes.ReplaceAll(out, []byte(`{`), []byte(`{"transformed":true,`))
			return out, nil
		},
		injectAuth: func(req *http.Request, creds []byte) error {
			req.Header.Del("anthropic-version")
			req.Header.Set("api-key", string(creds))
			req.Header.Del("Authorization")
			return nil
		},
	}

	v := &stubVault{}
	_ = v.Set(context.Background(), "user1", "fake", []byte("vault-stored-fake-key"))

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{
		AnthropicBaseURL: "https://should-not-be-called.example",
		AnthropicFlavor:  flavor,
	}

	body := []byte(`{"model":"claude-opus-4-7","max_tokens":1024}`)
	inbound := httptest.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewReader(body))
	inbound.Header.Set("Authorization", "Bearer cvis_xxx")
	inbound.Header.Set("anthropic-version", "2023-06-01")

	resp, err := f.Forward(context.Background(), "user1", "", conversation.ProviderAnthropic, inbound, body)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if seenPath != "/v1/messages" {
		t.Errorf("upstream path=%q want /v1/messages", seenPath)
	}
	if seenAPIKey != "vault-stored-fake-key" {
		t.Errorf("api-key=%q want vault-stored-fake-key", seenAPIKey)
	}
	if seenAuth != "" {
		t.Errorf("Authorization should have been cleared; got %q", seenAuth)
	}
	if seenAnthropicVersion != "" {
		t.Errorf("anthropic-version header should be stripped by flavor; got %q", seenAnthropicVersion)
	}
	if !bytes.Contains(seenBody, []byte(`"transformed":true`)) {
		t.Errorf("body not transformed by flavor: %s", seenBody)
	}
	if bytes.Contains(seenBody, []byte(`"model":"claude-opus-4-7"`)) {
		t.Errorf("model field leaked through transform: %s", seenBody)
	}
	if flavor.gotModel != "claude-opus-4-7" {
		t.Errorf("model passed to BuildURL=%q", flavor.gotModel)
	}
}

func TestForward_FlavorRespectsPassthroughMode(t *testing.T) {
	t.Parallel()

	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	flavor := &simpleFlavor{
		name: "fake",
		buildURL: func(path, model string) (*url.URL, error) {
			return url.Parse(upstream.URL + path)
		},
		transformBody: func(b []byte) ([]byte, error) { return b, nil },
		// Passthrough: don't touch Authorization.
		injectAuth: func(req *http.Request, _ []byte) error { return nil },
	}

	v := &stubVault{}
	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicFlavor: flavor}

	inbound := httptest.NewRequest(http.MethodPost, "/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"m"}`)))
	inbound.Header.Set("Authorization", "Bearer passthrough-vertex-token")

	ctx := WithPassthroughUpstreamAuth(context.Background())
	resp, err := f.Forward(ctx, "user1", "", conversation.ProviderAnthropic, inbound,
		[]byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if seenAuth != "Bearer passthrough-vertex-token" {
		t.Errorf("upstream Authorization=%q want passthrough", seenAuth)
	}
}

func TestForward_NativeAnthropicUnchangedWhenFlavorNil(t *testing.T) {
	t.Parallel()
	// With AnthropicFlavor nil, the pre-existing native path is taken.
	v := &stubVault{}
	_ = v.Set(context.Background(), "u", "anthropic", []byte("sk-ant-real"))

	var seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicBaseURL: upstream.URL}

	inbound := httptest.NewRequest(http.MethodPost, "/api/v1/messages",
		bytes.NewReader([]byte(`{}`)))
	resp, err := f.Forward(context.Background(), "u", "", conversation.ProviderAnthropic, inbound, []byte(`{}`))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if seenAPIKey != "sk-ant-real" {
		t.Errorf("native path broken: x-api-key=%q", seenAPIKey)
	}
}

func TestForward_FlavorPassthroughOnlyRequiresAuth(t *testing.T) {
	t.Parallel()
	// Flavor with empty VaultServiceID and no inbound passthrough
	// bearer should return a clear configuration error rather than
	// silently calling the upstream with no auth.
	flavor := &simpleFlavor{
		name:           "passthrough-only",
		vaultServiceID: "", // signals passthrough-only
		buildURL: func(path, model string) (*url.URL, error) {
			return url.Parse("https://nowhere.example" + path)
		},
		transformBody: func(b []byte) ([]byte, error) { return b, nil },
		injectAuth:    func(*http.Request, []byte) error { return nil },
	}
	v := &stubVault{}
	f := NewForwarder(v)
	f.Upstream = UpstreamSelector{AnthropicFlavor: flavor}

	inbound := httptest.NewRequest(http.MethodPost, "/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"m"}`)))
	// No Authorization on inbound, no PassthroughUpstreamAuth context.

	if _, err := f.Forward(context.Background(), "u", "", conversation.ProviderAnthropic, inbound,
		[]byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected error for passthrough-only flavor with no inbound bearer")
	}
}
