package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyPassthroughAnthropic walks Mode B: agent presents the
// Clawvisor token in X-Clawvisor-Agent-Token (out-of-band) and the user's
// real sign-in token in Authorization. Proxy preserves Authorization
// verbatim and strips x-api-key.
func TestLLMProxyPassthroughAnthropic(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "passthrough-agent"}, &agent)

	// Mode B: token in X-Clawvisor-Agent-Token, real bearer in Authorization.
	const upstreamBearer = "Bearer ant-passthrough-test-token-abc"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", upstreamBearer)
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

	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	got := upstream.Last()

	// Authorization preserved verbatim.
	if got.Headers.Get("Authorization") != upstreamBearer {
		t.Fatalf("upstream Authorization=%q, want %q", got.Headers.Get("Authorization"), upstreamBearer)
	}
	// x-api-key stripped (Clawvisor agent token must not leak).
	if got.Headers.Get("x-api-key") != "" {
		t.Fatalf("upstream got x-api-key=%q, want empty", got.Headers.Get("x-api-key"))
	}
	// anthropic-version still set.
	if got.Headers.Get("anthropic-version") == "" {
		t.Fatalf("upstream missing anthropic-version header")
	}
}

// TestLLMProxyPassthroughRejectsClawvisorToken — if the inbound Authorization
// is itself a cvis_* token (the agent's own), passthrough mode must NOT
// forward it to upstream. The proxy treats it as no upstream credential.
func TestLLMProxyPassthroughRejectsClawvisorToken(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "p2"}, &agent)

	// Authorization carries the agent's own cvis_* token — should NOT be
	// forwarded as the upstream bearer.
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", "Bearer "+agent.Token) // cvis_ prefix
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()

	// The proxy should NOT have called upstream with the cvis_ token.
	if upstream.Count() > 0 {
		got := upstream.Last()
		if got.Headers.Get("Authorization") == "Bearer "+agent.Token {
			t.Fatalf("clawvisor token leaked to upstream: %q", got.Headers.Get("Authorization"))
		}
	}
}
