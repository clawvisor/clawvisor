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
// Clawvisor token in X-Clawvisor-Agent-Token (out-of-band) and the
// user's real upstream bearer in Authorization. The proxy must:
//
//   - preserve Authorization verbatim,
//   - strip X-Clawvisor-Agent-Token before forwarding (internal-only
//     header — leaking it gives upstream the Clawvisor agent token),
//   - strip any inbound x-api-key (Anthropic-style key header — the
//     caller is going through Mode B, NOT injecting their own key),
//   - still set anthropic-version.
//
// We send a sentinel x-api-key in the inbound request so the strip
// check has actual coverage (an empty inbound would make the assertion
// vacuously true).
func TestLLMProxyPassthroughAnthropic(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)

	// Spec 02: header-driven passthrough is disabled under the default vault
	// posture (the F1 fix); passthrough is now a server-side posture. Opt in
	// via the upstream_auth knob so this mode-B lane keeps its coverage.
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "passthrough-agent"}, &agent)

	// Mode B: token in X-Clawvisor-Agent-Token, real bearer in Authorization.
	// Inbound x-api-key is a sentinel the proxy must strip — keeping it
	// in here gives the strip assertion below real coverage.
	const upstreamBearer = "Bearer ant-passthrough-test-token-abc"
	const sentinelXAPIKey = "sk-ant-INBOUND-SENTINEL-must-not-leak"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("Authorization", upstreamBearer)
	req.Header.Set("x-api-key", sentinelXAPIKey)
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
	// X-Clawvisor-Agent-Token stripped — leaking this hands the upstream
	// our internal agent identity.
	if leak := got.Headers.Get("X-Clawvisor-Agent-Token"); leak != "" {
		t.Fatalf("upstream got leaked X-Clawvisor-Agent-Token=%q, want empty", leak)
	}
	// x-api-key from the inbound request was stripped (sentinel must
	// not appear upstream).
	if k := got.Headers.Get("x-api-key"); k != "" {
		t.Fatalf("upstream got x-api-key=%q, want empty (inbound sentinel %q leaked)", k, sentinelXAPIKey)
	}
	// anthropic-version still set.
	if got.Headers.Get("anthropic-version") == "" {
		t.Fatalf("upstream missing anthropic-version header")
	}
}

// TestLLMProxyPassthroughRejectsClawvisorToken — if the inbound
// Authorization is itself a cvis_* token (the agent's own), passthrough
// mode must NOT forward it to upstream. The proxy treats this as
// "no upstream credential" — and since there's also no vault key for
// this user, the proxy surfaces a synthetic key-missing error in a
// 200-wrapped envelope (the streaming-friendly convention) rather than
// hitting upstream.
//
// Asserts both:
//
//   - the response status is 200 (envelope convention, NOT 500/403),
//   - if upstream was reached at all, the cvis_ token did NOT leak as
//     the Authorization bearer.
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

	body, _ := io.ReadAll(resp.Body)
	// 200 with an envelope is the streaming-friendly contract; 5xx/403
	// would indicate a behavior regression.
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s (expected 200 with key-missing envelope)", resp.StatusCode, body)
	}

	// The proxy should NOT have called upstream with the cvis_ token.
	if upstream.Count() > 0 {
		got := upstream.Last()
		if got.Headers.Get("Authorization") == "Bearer "+agent.Token {
			t.Fatalf("clawvisor token leaked to upstream Authorization=%q", got.Headers.Get("Authorization"))
		}
	}
}
