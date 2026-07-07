package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxySharedVaultInjected proves the instance-shared vault flow
// (spec 04 §C): an admin sets one Anthropic key under `_instance` via
// PUT /api/vault/shared/{serviceID}, and proxy-lite key injection uses it
// for an agent whose owner has NO personal Anthropic key — the shared key
// resolves as a fallback through InstanceAwareVault and lands as the
// upstream x-api-key.
func TestLLMProxySharedVaultInjected(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	// The local user is the instance admin (magic-local operator); it has no
	// personal Anthropic key, so injection must fall back to the shared one.
	admin := cv.LoginAsLocalUser(t)

	const sharedKey = "sk-ant-shared-instance-key-67890"
	cvPut(t, cv, admin.AccessToken, "/api/vault/shared/anthropic",
		map[string]any{"credential": sharedKey}, nil)

	// Agent owned by the admin user (which holds NO personal anthropic key).
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, admin.AccessToken, "/api/agents", map[string]any{"name": "shared-vault-agent"}, &agent)

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

	if c := upstream.Count(); c != 1 {
		t.Fatalf("upstream got %d requests, want 1", c)
	}
	got := upstream.Last()
	if k := got.Headers.Get("x-api-key"); k != sharedKey {
		t.Fatalf("upstream x-api-key=%q, want shared key %q", k, sharedKey)
	}
	if auth := got.Headers.Get("Authorization"); auth != "" {
		t.Fatalf("upstream got leaked Authorization=%q, want empty", auth)
	}
}
