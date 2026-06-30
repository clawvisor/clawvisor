package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyVaultInjectedAnthropic walks the Mode A path:
//
//   agent → clawvisor /api/v1/messages (Authorization: cvis_<token>)
//     → forwarder looks up vault key for "anthropic"
//     → upstream gets x-api-key=<vault key>, no Authorization, anthropic-version set
//
// Cassette server stands in for api.anthropic.com via the new
// CLAWVISOR_LLM_UPSTREAM_ANTHROPIC env override.
func TestLLMProxyVaultInjectedAnthropic(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	// Seed user-scope vault entry via the dedicated LLM credentials endpoint
	// (the generic /api/vault/items rejects "anthropic" as a reserved id).
	const vaultKey = "sk-ant-test-vault-anthropic-key-12345"
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", vaultKey)

	// Create an agent.
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "proxy-test-agent"}, &agent)

	// Agent sends a /api/v1/messages request. Mode A is selected by
	// presenting the agent's cvis_* token in Authorization (no separate
	// upstream bearer, no x-api-key) — the proxy recognizes the cvis_
	// prefix, looks up the vault key for "anthropic", and injects it
	// as x-api-key upstream while stripping the original Authorization.
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// Upstream got exactly one request.
	if c := upstream.Count(); c != 1 {
		t.Fatalf("upstream got %d requests, want 1", c)
	}
	got := upstream.Last()

	// Path was preserved → /v1/messages on upstream.
	if got.Path != "/v1/messages" {
		t.Fatalf("upstream path=%q, want /v1/messages", got.Path)
	}

	// x-api-key is the vault key.
	if k := got.Headers.Get("x-api-key"); k != vaultKey {
		t.Fatalf("upstream x-api-key=%q, want %q", k, vaultKey)
	}

	// Authorization was stripped (no agent token leakage).
	if auth := got.Headers.Get("Authorization"); auth != "" {
		t.Fatalf("upstream got leaked Authorization=%q, want empty", auth)
	}

	// anthropic-version was set.
	if v := got.Headers.Get("anthropic-version"); v == "" {
		t.Fatalf("upstream missing anthropic-version header")
	}
}

// TestLLMProxyVaultMissingReturnsError — no vault entry → 500/UPSTREAM_KEY_MISSING.
func TestLLMProxyVaultMissingReturnsError(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct{ Token string }
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "no-vault-agent"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	// clawvisor surfaces upstream-key-missing as a 200 with an error envelope
	// in the body (streaming-friendly), not an HTTP error status. The
	// upstream itself must not be reached.
	if upstream.Count() != 0 {
		t.Fatalf("upstream should NOT have been called; got %d hits", upstream.Count())
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := strings.ToLower(string(body))
	// Proxy returns a synthetic "Clawvisor: no Anthropic API key configured"
	// assistant message so the agent sees a guiding error instead of a raw 401.
	if !strings.Contains(bodyStr, "no anthropic api key") &&
		!strings.Contains(bodyStr, "upstream_key_missing") &&
		!strings.Contains(bodyStr, "vault") &&
		!strings.Contains(bodyStr, "configured") {
		t.Fatalf("body doesn't indicate vault miss: %s", body)
	}
}

// TestLLMProxyAgentScopedVaultPreferred — agent-scoped key wins over user-scoped.
func TestLLMProxyAgentScopedVaultPreferred(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	// User-scope key (fallback).
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-USER-SCOPE-KEY")

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "scoped-agent"}, &agent)

	// Agent-scope key — should be preferred.
	llmCredSet(t, cv, user.AccessToken, "anthropic", agent.ID, "sk-ant-AGENT-SCOPE-KEY")

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	if got := upstream.Last().Headers.Get("x-api-key"); got != "sk-ant-AGENT-SCOPE-KEY" {
		t.Fatalf("x-api-key=%q, want AGENT-SCOPE-KEY (agent scope should win)", got)
	}
}

