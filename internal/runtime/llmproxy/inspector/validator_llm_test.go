package inspector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/config"
)

// Validator should fail closed (ambiguous=true) when verification is
// disabled — preserves the old AmbiguousValidator behavior so installs
// without LLM credentials don't suddenly authorize unparseable shapes.
func TestLLMClientValidator_DisabledFailsClosed(t *testing.T) {
	v := NewLLMClientValidator(func() config.VerificationConfig {
		return config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{Enabled: false},
		}
	}, nil)
	got, err := v.Validate(context.Background(), ToolUse{Name: "Custom", Input: []byte(`{"x":"autovault_x_y"}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Ambiguous {
		t.Errorf("expected ambiguous=true when verification disabled, got %+v", got)
	}
}

// When verification is enabled but no API key / project is set, the
// validator can't reach an LLM — must fail closed instead of letting
// llm.Client crash mid-call.
func TestLLMClientValidator_MissingCredentialsFailsClosed(t *testing.T) {
	v := NewLLMClientValidator(func() config.VerificationConfig {
		return config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:  true,
				Provider: "anthropic",
				// APIKey deliberately empty
			},
		}
	}, nil)
	got, _ := v.Validate(context.Background(), ToolUse{Name: "Custom", Input: []byte(`{}`)})
	if !got.Ambiguous {
		t.Errorf("expected ambiguous=true on missing api key, got %+v", got)
	}
	if !strings.Contains(got.Reason, "provider not configured") {
		t.Errorf("reason should mention misconfig, got %q", got.Reason)
	}
}

// Provider-agnostic happy path: configure the validator to talk to an
// httptest server (pretending to be Anthropic), confirm the LLM call
// flows and the response parses into a Verdict.
func TestLLMClientValidator_HappyPath_Anthropic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("expected api key header, got %q", r.Header.Get("x-api-key"))
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"{\"is_api_call\":true,\"ambiguous\":false,\"method\":\"GET\",\"host\":\"api.github.com\",\"path\":\"/user\",\"credential_locations\":[{\"kind\":\"header\",\"name\":\"Authorization\",\"scheme\":\"Bearer\"}],\"reason\":\"clean\"}"}]
		}`))
	}))
	defer upstream.Close()

	v := NewLLMClientValidator(func() config.VerificationConfig {
		return config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "anthropic",
				Endpoint:       upstream.URL,
				APIKey:         "test-key",
				Model:          "claude-haiku-4-5",
				TimeoutSeconds: 10,
			},
		}
	}, nil)
	got, err := v.Validate(context.Background(), ToolUse{
		Name:  "Custom",
		Input: json.RawMessage(`{"url":"https://api.github.com/user","headers":{"Authorization":"Bearer autovault_github_xxx"}}`),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !got.IsAPICall {
		t.Errorf("expected IsAPICall=true; reason=%q", got.Reason)
	}
	if got.Host != "api.github.com" {
		t.Errorf("host=%q, want api.github.com", got.Host)
	}
	if got.Method != "GET" {
		t.Errorf("method=%q, want GET", got.Method)
	}
}

// LLM transport errors must be converted to ambiguous=true so the
// rewriter fails closed rather than acting on a partial verdict.
func TestLLMClientValidator_TransportErrorFailsClosed(t *testing.T) {
	v := NewLLMClientValidator(func() config.VerificationConfig {
		return config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "anthropic",
				Endpoint:       "http://127.0.0.1:1", // refused
				APIKey:         "test-key",
				TimeoutSeconds: 1,
			},
		}
	}, nil)
	got, err := v.Validate(context.Background(), ToolUse{Name: "Custom", Input: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Validate should return ambiguous-not-error: %v", err)
	}
	if !got.Ambiguous {
		t.Errorf("expected ambiguous on transport error, got %+v", got)
	}
}

// parseValidatorJSON should tolerate a model that wraps the JSON in
// ```json fences (Gemini sometimes does this even when told not to).
func TestParseValidatorJSON_StripsCodeFences(t *testing.T) {
	raw := "```json\n" + `{"is_api_call":true,"ambiguous":false,"method":"POST","host":"api.example.com","path":"/x"}` + "\n```"
	got, err := parseValidatorJSON(raw)
	if err != nil {
		t.Fatalf("parseValidatorJSON: %v", err)
	}
	if !got.IsAPICall || got.Host != "api.example.com" {
		t.Errorf("parse mismatch: %+v", got)
	}
}

// is_api_call=true with empty method must flip to ambiguous so we
// don't silently default to GET in audit + policy decisions.
func TestParseValidatorJSON_MissingMethodForcesAmbiguous(t *testing.T) {
	raw := `{"is_api_call":true,"ambiguous":false,"method":"","host":"api.example.com","path":"/x"}`
	got, err := parseValidatorJSON(raw)
	if err != nil {
		t.Fatalf("parseValidatorJSON: %v", err)
	}
	if !got.Ambiguous {
		t.Errorf("expected ambiguous=true when method is empty but is_api_call=true; got %+v", got)
	}
}

// Vertex / Gemini provider gating: presence of Project is what matters,
// not APIKey (those providers use ADC).
func TestLLMProviderConfigured_VertexGeminiUsesProject(t *testing.T) {
	yes := config.LLMProviderConfig{Provider: "gemini", Project: "clawvisor-staging"}
	if !llmProviderConfigured(yes) {
		t.Errorf("gemini with project should be configured")
	}
	no := config.LLMProviderConfig{Provider: "gemini"}
	if llmProviderConfigured(no) {
		t.Errorf("gemini without project should be unconfigured")
	}
}
