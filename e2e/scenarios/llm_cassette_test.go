package scenarios_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
	hllm "github.com/clawvisor/clawvisor/testharness/llm"
)

// TestClawvisorBootsWithCassetteLLMVerificationEndpoint — boot-time
// smoke that the CLAWVISOR_LLM_VERIFICATION_* env vars (ENABLED,
// PROVIDER, ENDPOINT, API_KEY, MODEL) are accepted by clawvisor-server
// without crashing, with ENDPOINT pointed at a cassette-backed HTTP
// stand-in. Login is then exercised to confirm the rest of the
// bootstrap path still works under those env vars.
//
// Scope: wiring only. The full "request → verifier → cassette →
// response" flow is covered by
// TestLLMProxyDoesNotBlockInScopeLocalToolUse (and the suite of
// llm_proxy_intent_verify_test.go scenarios) which exercises actual
// verification calls through the same env-var surface.
func TestClawvisorBootsWithCassetteLLMVerificationEndpoint(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_x","role":"assistant","content":[{"type":"text","text":"{\"verdict\":\"allow\"}"}]}`))
	}))
	defer fakeUpstream.Close()

	dir := t.TempDir()
	cassette := hllm.NewCassette(dir, t.Name(), hllm.ModePassthrough)
	server := hllm.NewServer(t, cassette, fakeUpstream.URL)

	h := testharness.New(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_VERIFICATION_ENABLED":  "true",
		"CLAWVISOR_LLM_VERIFICATION_PROVIDER": "anthropic",
		"CLAWVISOR_LLM_VERIFICATION_ENDPOINT": server.URL(),
		"CLAWVISOR_LLM_VERIFICATION_API_KEY":  "sk-ant-test-key",
		"CLAWVISOR_LLM_VERIFICATION_MODEL":    "claude-haiku-4-5-20251001",
	})

	user := cv.LoginAsLocalUser(t)
	if user.AccessToken == "" {
		t.Fatal("login: empty token (clawvisor bootstrap regressed under verification env)")
	}
	if !strings.HasPrefix(server.URL(), "http://") {
		t.Fatalf("cassette server URL malformed: %q", server.URL())
	}
}
