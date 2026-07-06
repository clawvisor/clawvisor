package scenarios_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyOpenAIAPIKeyRoute — Mode B passthrough with an sk-* OpenAI
// API key → routes to api.openai.com (not chatgpt.com).
func TestLLMProxyOpenAIAPIKeyRoute(t *testing.T) {
	h := testharness.New(t)
	openaiCapture := newUpstreamCapture(t)
	chatgptCapture := newUpstreamCapture(t)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI":  openaiCapture.URL(),
		"CLAWVISOR_LLM_UPSTREAM_CHATGPT": chatgptCapture.URL(),
		// Spec 02 §4b: header placement no longer selects passthrough; the
		// default posture is vault. These Mode B route tests opt into
		// passthrough explicitly to keep their client-key routing coverage.
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "openai-key"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5","input":"hi"}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", "Bearer sk-test-openai-key-123")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/responses: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	_ = body

	if openaiCapture.Count() != 1 {
		t.Fatalf("api.openai.com hits=%d, want 1; body=%s", openaiCapture.Count(), body)
	}
	if chatgptCapture.Count() != 0 {
		t.Fatalf("chatgpt.com hits=%d, want 0 (sk-* key should route to api.openai.com)", chatgptCapture.Count())
	}
}

// TestLLMProxyChatGPTOAuthRoute — Mode B passthrough with a ChatGPT-OAuth
// JWT (no api.responses.write scope) → routes to chatgpt.com.
func TestLLMProxyChatGPTOAuthRoute(t *testing.T) {
	h := testharness.New(t)
	openaiCapture := newUpstreamCapture(t)
	chatgptCapture := newUpstreamCapture(t)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI":  openaiCapture.URL(),
		"CLAWVISOR_LLM_UPSTREAM_CHATGPT": chatgptCapture.URL(),
		// Spec 02 §4b: header placement no longer selects passthrough; the
		// default posture is vault. These Mode B route tests opt into
		// passthrough explicitly to keep their client-key routing coverage.
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "chatgpt-oauth"}, &agent)

	// Craft a synthetic ChatGPT-OAuth JWT: scp is a JSON array WITHOUT
	// api.responses.write. The signature can be anything since the mock
	// upstream doesn't verify it.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"scp":["openid","email"]}`))
	jwt := header + "." + payload + ".sig"

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5","input":"hi"}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/responses: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	_ = body

	if chatgptCapture.Count() != 1 {
		t.Fatalf("chatgpt.com hits=%d, want 1 (ChatGPT-OAuth JWT should route here); body=%s",
			chatgptCapture.Count(), body)
	}
	if openaiCapture.Count() != 0 {
		t.Fatalf("api.openai.com hits=%d, want 0", openaiCapture.Count())
	}

	// Path mapping: /api/v1/responses → /backend-api/codex/responses.
	got := chatgptCapture.Last()
	if got.Path != "/backend-api/codex/responses" {
		t.Fatalf("chatgpt.com path=%q, want /backend-api/codex/responses", got.Path)
	}
}

// TestLLMProxyOpenAIScopedJWTRoute — JWT WITH api.responses.write goes to
// api.openai.com, not chatgpt.com.
func TestLLMProxyOpenAIScopedJWTRoute(t *testing.T) {
	h := testharness.New(t)
	openaiCapture := newUpstreamCapture(t)
	chatgptCapture := newUpstreamCapture(t)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI":  openaiCapture.URL(),
		"CLAWVISOR_LLM_UPSTREAM_CHATGPT": chatgptCapture.URL(),
		// Spec 02 §4b: header placement no longer selects passthrough; the
		// default posture is vault. These Mode B route tests opt into
		// passthrough explicitly to keep their client-key routing coverage.
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "scoped-jwt"}, &agent)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"scp":["openid","api.responses.write"]}`))
	jwt := header + "." + payload + ".sig"

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5","input":"hi"}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if openaiCapture.Count() != 1 {
		t.Fatalf("api.openai.com hits=%d, want 1 (JWT with api.responses.write should route here)", openaiCapture.Count())
	}
	if chatgptCapture.Count() != 0 {
		t.Fatalf("chatgpt.com hits=%d, want 0", chatgptCapture.Count())
	}
}
