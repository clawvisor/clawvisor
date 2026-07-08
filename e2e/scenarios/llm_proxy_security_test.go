package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyCallerAuthSourceClawvisorHeader — when the agent token
// arrives in X-Clawvisor-Agent-Token, the request runs in passthrough
// mode (no vault lookup, the inbound Authorization is forwarded
// verbatim). The audit log records caller_auth_source=x-clawvisor-agent-token.
func TestLLMProxyCallerAuthSourceClawvisorHeader(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
		// Spec 02 §4b: the default posture is vault, which overrides the
		// middleware's header-derived passthrough flag. This test asserts
		// passthrough (caller_auth_source + forwarded bearer), so it opts
		// into passthrough upstream auth explicitly.
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cas-clawvisor"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", "Bearer real-anthropic-bearer-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	// Passthrough behavior: upstream got the real bearer, not a vault key.
	if upstream.Last().Headers.Get("Authorization") != "Bearer real-anthropic-bearer-token" {
		t.Fatalf("passthrough not honored; upstream Authorization=%q", upstream.Last().Headers.Get("Authorization"))
	}
}

// TestLLMProxyCallerAuthSourceAuthorization — agent token in Authorization
// header (the conventional client SDK pattern). Triggers Mode A vault lookup.
func TestLLMProxyCallerAuthSourceAuthorization(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-cas-test-key")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cas-authz"}, &agent)

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
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	// Vault-injected behavior: upstream got x-api-key = vault value, Authorization stripped.
	if upstream.Last().Headers.Get("x-api-key") != "sk-ant-cas-test-key" {
		t.Fatalf("x-api-key wrong: %q", upstream.Last().Headers.Get("x-api-key"))
	}
	if upstream.Last().Headers.Get("Authorization") != "" {
		t.Fatalf("Authorization should be stripped on vault path; got %q", upstream.Last().Headers.Get("Authorization"))
	}
}

// TestLLMProxyCallerAuthSourceXAPIKey — agent token in x-api-key header
// (the Anthropic SDK convention). Still triggers vault lookup (Mode A).
func TestLLMProxyCallerAuthSourceXAPIKey(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-xap-test-key")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cas-xap"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("x-api-key", agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	// Should still vault-inject (not passthrough — passthrough requires the
	// out-of-band X-Clawvisor-Agent-Token header).
	if upstream.Last().Headers.Get("x-api-key") != "sk-ant-xap-test-key" {
		t.Fatalf("x-api-key wrong: %q", upstream.Last().Headers.Get("x-api-key"))
	}
}

// TestLLMProxyMissingAuth — no auth header at all → 401.
func TestLLMProxyMissingAuth(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 401", resp.StatusCode, body)
	}
}

// TestLLMProxyBogusAgentToken — non-existent token → 401.
func TestLLMProxyBogusAgentToken(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer cvis_definitely_not_a_real_token_value")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 401", resp.StatusCode, body)
	}
}

// silence unused import warnings during refactors
var _ = strings.Contains
var _ = time.Now
