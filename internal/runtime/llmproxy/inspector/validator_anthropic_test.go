package inspector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicValidator_PromptSHADeterministic(t *testing.T) {
	v := &AnthropicValidator{}
	got := v.PromptSHA()
	if len(got) != 64 {
		t.Fatalf("expected 64-char SHA, got %d (%q)", len(got), got)
	}
	if got != v.PromptSHA() {
		t.Fatalf("PromptSHA must be deterministic")
	}
}

func TestAnthropicValidator_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// t.Fatalf inside an httptest handler goroutine calls Goexit on
		// the handler, not the test — leaves the test passing despite the
		// failure. Use Errorf + return so the assertion is recorded on the
		// test object and the handler returns cleanly.
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("expected x-api-key=test-key, got %q", r.Header.Get("x-api-key"))
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{
			"content":[
				{"type":"text","text":"{\"is_api_call\":true,\"ambiguous\":false,\"method\":\"POST\",\"host\":\"api.github.com\",\"path\":\"/repos/x/y\",\"credential_locations\":[{\"kind\":\"header\",\"name\":\"Authorization\",\"scheme\":\"Bearer\"}],\"reason\":\"clean curl\"}"}
			]
		}`))
	}))
	defer upstream.Close()

	v := &AnthropicValidator{
		APIKey: "test-key",
		Model:  "claude-haiku-4-5",
		HTTP:   upstream.Client(),
	}
	v.HTTP = redirectingClient(upstream.URL)

	got, err := v.Validate(context.Background(), toolUse("WeirdTool", `{"raw":"autovault_github_xxx"}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !got.IsAPICall || got.Ambiguous {
		t.Fatalf("expected IsAPICall + non-ambiguous, got %+v", got)
	}
	if got.Host != "api.github.com" || got.Method != "POST" {
		t.Fatalf("verdict mismatch: %+v", got)
	}
}

func TestAnthropicValidator_FallsBackToAmbiguousOnTransportError(t *testing.T) {
	v := &AnthropicValidator{
		APIKey: "test-key",
		HTTP:   &http.Client{Transport: failTransport{}},
	}
	got, err := v.Validate(context.Background(), toolUse("WeirdTool", `{"raw":"autovault_github_xxx"}`))
	if err != nil {
		t.Fatalf("Validate should not return errors; converts to ambiguous")
	}
	if !got.Ambiguous {
		t.Fatalf("expected ambiguous on transport error, got %+v", got)
	}
}

func TestAnthropicValidator_HandlesCodeFences(t *testing.T) {
	body := `{
		"content":[
			{"type":"text","text":"` + "```json" + `\n{\"is_api_call\":false,\"ambiguous\":false,\"reason\":\"placeholder in echo\"}\n` + "```" + `"}
		]
	}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	v := &AnthropicValidator{APIKey: "test-key", HTTP: redirectingClient(upstream.URL)}

	got, err := v.Validate(context.Background(), toolUse("Bash", `{"cmd":"echo autovault_github_xxx"}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.IsAPICall {
		t.Fatalf("expected IsAPICall=false, got %+v", got)
	}
}

// --- helpers ---

// redirectingClient swaps every outbound request URL to point at base,
// so Validate's hard-coded api.anthropic.com URL hits the test server.
func redirectingClient(base string) *http.Client {
	return &http.Client{
		Transport: redirectTransport{base: base},
	}
}

type redirectTransport struct{ base string }

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(req.URL.String(), "https://api.anthropic.com") {
		return http.DefaultTransport.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	u := req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(rt.base, "http://")
	clone.URL = u
	return http.DefaultTransport.RoundTrip(clone)
}

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrHandlerTimeout
}

// guard against compile drift on the canned response shape.
var _ = json.RawMessage(nil)
