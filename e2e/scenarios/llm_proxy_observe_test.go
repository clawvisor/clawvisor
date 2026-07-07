package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// llm_proxy_observe_test.go covers spec 02 §3: the Observe posture's
// enforcement-off mode. The pipeline still inspects, attributes, audits,
// and meters cost, but every would-be enforcing verdict is downgraded to
// a recorded observation and the request/response proceeds unmodified.

// featuresProxyLiteEnabled reports the /api/features proxy_lite flag.
func featuresProxyLiteEnabled(t *testing.T, cv *testapp.Server) bool {
	t.Helper()
	resp := cvDo(t, cv, "", "GET", "/api/features", nil)
	defer resp.Body.Close()
	var feats struct {
		ProxyLite bool `json:"proxy_lite"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&feats)
	return feats.ProxyLite
}

// outOfScopeToolUseUpstream returns a fake upstream whose response
// contains a credentialed tool_use (github.list_issues) that no active
// task covers — the deterministic scope-drift enforcement fixture. Under
// enforce the proxy rewrites it to CLAWVISOR_BLOCKED; under observe it
// must pass through unmodified.
func outOfScopeToolUseUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_oos",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [
    {"type": "text", "text": "I'll list the open issues."},
    {"type": "tool_use", "id": "toolu_oos", "name": "github.list_issues", "input": {"owner": "x", "repo": "y"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 50, "output_tokens": 30}
}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// driveOutOfScopeToolUse boots a server with the given enforcement_mode,
// activates github, creates an agent with no covering task, and issues
// one /api/v1/messages request whose upstream response carries the
// out-of-scope tool_use. Returns the response body string and the
// user/agent tokens for follow-up audit assertions.
func driveOutOfScopeToolUse(t *testing.T, enforcementMode string) (respBody string, cv *testapp.Server, userToken string) {
	t.Helper()
	h := testharness.New(t)
	upstream := outOfScopeToolUseUpstream(t)

	env := map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
		"GITHUB_API_BASE_URL":              h.GitHub.URL(),
	}
	if enforcementMode != "" {
		env["CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE"] = enforcementMode
	}
	cv = testapp.StartWith(t, h, env)
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "observe-oos"}, &agent)

	body := []byte(`{
  "model": "claude-haiku-4-5-20251001",
  "max_tokens": 256,
  "messages": [{"role": "user", "content": "list issues"}]
}`)
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, rb)
	}
	return string(rb), cv, user.AccessToken
}

// TestObservePresetAppliesKnobs: a config carrying only `posture: observe`
// boots proxy-lite enabled, with passthrough upstream auth and observe
// (enforcement-off) mode. Asserted via /api/features + a probe request.
func TestObservePresetAppliesKnobs(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWithConfig(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	}, "posture: observe")

	if !featuresProxyLiteEnabled(t, cv) {
		t.Fatal("posture: observe did not enable proxy-lite (/api/features proxy_lite=false)")
	}

	user := cv.LoginAsLocalUser(t)
	_, token := newPostureAgent(t, cv, user.AccessToken, "observe-preset")

	// upstream_auth=passthrough (from the preset): a request WITHOUT a
	// client provider credential is refused with PASSTHROUGH_NO_CREDENTIAL,
	// proving the preset selected passthrough (not the vault default).
	noCred := postureAgentReq(t, cv, token, nil)
	defer noCred.Body.Close()
	if noCred.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(readBodyStr(noCred), "PASSTHROUGH_NO_CREDENTIAL") {
		t.Fatalf("observe preset should select passthrough auth; got status=%d", noCred.StatusCode)
	}

	// With a client credential the request forwards it unchanged.
	withCred := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": "Bearer sk-ant-oat01-subscription",
	})
	defer withCred.Body.Close()
	if withCred.StatusCode != 200 {
		t.Fatalf("passthrough with client cred: status=%d body=%s", withCred.StatusCode, readBodyStr(withCred))
	}
	if got := upstream.Last().Headers.Get("Authorization"); got != "Bearer sk-ant-oat01-subscription" {
		t.Fatalf("passthrough altered client credential: %q", got)
	}
}

// TestKnobOverridesPreset: `posture: observe` plus an explicit
// enforcement_mode override (env) must let the explicit knob win — the
// scope-drift verdict still enforces (CLAWVISOR_BLOCKED). upstream_auth is
// also pinned to vault so the credentialed fixture runs without a client
// key; the assertion targets enforcement_mode precedence.
func TestKnobOverridesPreset(t *testing.T) {
	h := testharness.New(t)
	upstream := outOfScopeToolUseUpstream(t)
	cv := testapp.StartWithConfig(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":      upstream.URL,
		"GITHUB_API_BASE_URL":                   h.GitHub.URL(),
		"CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE": "enforce",
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH":    "vault",
	}, "posture: observe")
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "knob-override"}, &agent)

	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":256,"messages":[{"role":"user","content":"list issues"}]}`)
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rb), "CLAWVISOR_BLOCKED") {
		t.Fatalf("explicit enforcement_mode=enforce should override posture: observe (expected CLAWVISOR_BLOCKED); body: %s", rb)
	}
}

// TestObserveModeToolUseHoldPassesThrough: a tool_use that would be
// scope-drift-blocked under enforce passes through unmodified under
// observe — proven by contrast with the enforce control run.
func TestObserveModeToolUseHoldPassesThrough(t *testing.T) {
	enforceBody, _, _ := driveOutOfScopeToolUse(t, "enforce")
	if !strings.Contains(enforceBody, "CLAWVISOR_BLOCKED") {
		t.Fatalf("control (enforce) run should block the out-of-scope tool_use; body: %s", enforceBody)
	}

	observeBody, _, _ := driveOutOfScopeToolUse(t, "observe")
	if strings.Contains(observeBody, "CLAWVISOR_BLOCKED") {
		t.Fatalf("observe mode must NOT rewrite the blocked tool_use; body: %s", observeBody)
	}
	if !strings.Contains(observeBody, "github.list_issues") {
		t.Fatalf("observe mode must pass the original tool_use through unmodified; body: %s", observeBody)
	}
}

// TestObserveModeDowngradesDeny: under observe the would-be block is
// recorded (audit observed: true, with the tool verdict detail) while the
// upstream response is returned unmodified.
func TestObserveModeDowngradesDeny(t *testing.T) {
	body, cv, userToken := driveOutOfScopeToolUse(t, "observe")
	if strings.Contains(body, "CLAWVISOR_BLOCKED") {
		t.Fatalf("observe must not enforce the deny; body: %s", body)
	}
	if !auditContains(t, cv, userToken, `"observed":true`) {
		t.Fatal("audit did not record the downgraded verdict as observed: true")
	}
	if !auditContains(t, cv, userToken, "github.list_issues") {
		t.Fatal("audit did not record the observed tool_use verdict detail (tool name)")
	}
}

// TestObserveModeStillRejectsBadToken: observe downgrades policy verdicts
// only — Clawvisor's own auth is never weakened. A bad agent token is
// still rejected with 401.
func TestObserveModeStillRejectsBadToken(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":      upstream.URL(),
		"CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE": "observe",
	})

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", "cvis_not-a-real-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("observe mode must still reject a bad token; status=%d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 0 {
		t.Fatalf("bad token reached upstream in observe mode: hits=%d", upstream.Count())
	}
}
