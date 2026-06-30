package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// Auth-matrix completion for the LLM-proxy lite-proxy routes (/api/v1/*).
// Gemini isn't reachable here — it lives behind runtime-proxy CONNECT,
// not the lite-proxy endpoints — so Mode A/B for Google aren't testable
// through this surface. The Forwarder's injectPassthroughGoogleAuth +
// x-goog-api-key handling is exercised by the runtime-proxy tests inside
// the clawvisor submodule.

// TestLLMProxyVaultInjectedOpenAI exercises Mode A for OpenAI: agent
// presents cvis_ token in Authorization, proxy looks up vault, injects
// "Authorization: Bearer <vault key>" upstream.
func TestLLMProxyVaultInjectedOpenAI(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	const vaultKey = "sk-openai-vault-test-key-12345"
	llmCredSet(t, cv, user.AccessToken, "openai", "", vaultKey)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "openai-vault"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	got := upstream.Last()
	wantAuth := "Bearer " + vaultKey
	if got.Headers.Get("Authorization") != wantAuth {
		t.Fatalf("upstream Authorization=%q, want %q", got.Headers.Get("Authorization"), wantAuth)
	}
	// x-api-key must not have leaked (that's Anthropic's slot).
	if got.Headers.Get("x-api-key") != "" {
		t.Fatalf("unexpected x-api-key=%q on OpenAI request", got.Headers.Get("x-api-key"))
	}
}
