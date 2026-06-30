package scenarios_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyForwardsAnthropicVersionHeader — when the caller sets
// anthropic-version, it passes through to upstream as-is (not overridden
// by the proxy's default fallback).
func TestLLMProxyForwardsAnthropicVersionHeader(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-version-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "version"}, &agent)

	const customVersion = "2025-01-15"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", customVersion)
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := upstream.Last().Headers.Get("anthropic-version"); got != customVersion {
		t.Fatalf("upstream anthropic-version=%q, want %q (caller's value should pass through, not be overridden)",
			got, customVersion)
	}
}

// TestLLMProxyDefaultsAnthropicVersion — when caller doesn't set
// anthropic-version, proxy fills in the default.
func TestLLMProxyDefaultsAnthropicVersion(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-default-version")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "dversion"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := upstream.Last().Headers.Get("anthropic-version"); got == "" {
		t.Fatalf("upstream missing anthropic-version (proxy should default)")
	}
}

// TestLLMProxyForwardsAnthropicBetaHeader — anthropic-beta passes through.
func TestLLMProxyForwardsAnthropicBetaHeader(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-beta-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "beta"}, &agent)

	const beta = "prompt-caching-2024-07-31,messages-2023-12-15"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", beta)
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := upstream.Last().Headers.Get("anthropic-beta"); got != beta {
		t.Fatalf("upstream anthropic-beta=%q, want %q", got, beta)
	}
}

// TestLLMProxyForwardsOpenAIOrgHeader — OpenAI-Organization passes
// through (multi-org accounts route to the right billing context).
func TestLLMProxyForwardsOpenAIOrgHeader(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "openai", "", "sk-org-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "org"}, &agent)

	const org = "org-abc12345"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Organization", org)
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := upstream.Last().Headers.Get("OpenAI-Organization"); got != org {
		t.Fatalf("upstream OpenAI-Organization=%q, want %q", got, org)
	}
}
