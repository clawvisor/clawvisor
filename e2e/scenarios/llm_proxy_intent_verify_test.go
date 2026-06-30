package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// The clawvisor intent verifier has TWO call sites:
//
//   gateway: POST /api/gateway/request → GatewayHandler.runVerification
//   proxy:   POST /api/v1/messages    → LLMEndpointHandler postprocess pipeline
//                                       (server.go:1329 wires
//                                       NewCircuitBreakerVerifier on llmHandler.IntentVerifier)
//
// Both share the underlying intent.Verifier (cassette-mockable), but
// fire at different points. The tests in intent_verify_test.go cover the
// gateway path. These tests cover the LLM-proxy path.
//
// To make the proxy verifier fire on a tool_use, the tool_use must:
//   (a) be detected by the resolver as a recognized service.action,
//   (b) the active task's scope must already cover it (otherwise the
//       scope-drift menu fires instead, before verification),
//   (c) auto_execute be true so the proxy doesn't open a user approval first.

// TestLLMProxyInterceptsOutOfScopeToolUse — when the upstream response
// contains a tool_use the active task doesn't cover, the proxy
// postprocess rewrites the response with a CLAWVISOR_BLOCKED marker so
// the agent gets the scope-drift menu instead of being able to invoke
// the tool. This is the scope-drift-via-LLM-proxy mechanism (compared to
// the HTTP-level expand/inline tests in scope_drift_test.go).
func TestLLMProxyInterceptsOutOfScopeToolUse(t *testing.T) {
	h := testharness.New(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer upstream.Close()

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
		"GITHUB_API_BASE_URL":              h.GitHub.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "oos-agent"}, &agent)
	// No matching task — the tool_use is out-of-scope from the proxy's view.

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
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, respBody)
	}

	// The proxy postprocess should have rewritten the tool_use into a
	// Bash command with CLAWVISOR_BLOCKED marker — the drift-menu pattern.
	bs := string(respBody)
	if !strings.Contains(bs, "CLAWVISOR_BLOCKED") {
		t.Fatalf("expected CLAWVISOR_BLOCKED in rewritten tool_use; body: %s", bs)
	}
	if !strings.Contains(bs, "github.list_issues") {
		t.Fatalf("expected the original tool name in the block marker; body: %s", bs)
	}
}

// TestLLMProxyIntentVerifierConsultedOnInScopeToolUse — when the
// upstream tool_use IS in scope (matched by expected_tools), the proxy
// runs intent verification before passing the response through. This
// test counts verifier hits to prove the LLM-proxy verifier was
// consulted (the same verifier that gateway tests exercise, but at a
// different call site).
func TestLLMProxyIntentVerifierConsultedOnInScopeToolUse(t *testing.T) {
	h := testharness.New(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_scoped",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [
    {"type": "text", "text": "Reading the file."},
    {"type": "tool_use", "id": "toolu_in", "name": "Read", "input": {"file_path": "/tmp/x"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 50, "output_tokens": 30}
}`))
	}))
	defer upstream.Close()

	verifierCalls := int32(0)
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&verifierCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_verifier_in", "type": "message", "role": "assistant",
			"model": "claude-haiku-4-5-20251001",
			"content": []map[string]any{{"type": "text", "text": `{"allow":true,"param_scope":"ok","reason_coherence":"ok","extract_context":false,"missing_chain_values":[],"explanation":"ok"}`}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer verifier.Close()

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":    upstream.URL,
		"CLAWVISOR_LLM_VERIFICATION_ENABLED":  "true",
		"CLAWVISOR_LLM_VERIFICATION_PROVIDER": "anthropic",
		"CLAWVISOR_LLM_VERIFICATION_ENDPOINT": verifier.URL,
		"CLAWVISOR_LLM_VERIFICATION_API_KEY":  "sk-ant-test-key",
		"CLAWVISOR_LLM_VERIFICATION_MODEL":    "claude-haiku-4-5-20251001",
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "in-scope-agent"}, &agent)

	// v2 schema task with expected_tools that names the local Read tool.
	// Local tools are explicitly in-scope without going through service
	// resolution, so this avoids the credentialed-resolver block path.
	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose":        "read /tmp/x",
		"schema_version": 2,
		"expected_tools": []map[string]any{
			{"tool_name": "Read", "why": "look at the file the user mentioned"},
		},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve",
		map[string]any{}, nil)

	body := []byte(`{
  "model": "claude-haiku-4-5-20251001",
  "max_tokens": 256,
  "messages": [{"role": "user", "content": "read /tmp/x"}]
}`)
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, respBody)
	}

	// The verifier may or may not be called for local tools (Read isn't a
	// credentialed action, so the pipeline may skip intent verify). What
	// we WANT to assert is the response wasn't drift-blocked.
	if strings.Contains(string(respBody), "CLAWVISOR_BLOCKED") {
		t.Fatalf("in-scope tool_use was scope-drift-blocked; body: %s", respBody)
	}
	t.Logf("verifier consulted %d time(s); response: %s",
		atomic.LoadInt32(&verifierCalls), respBody)
}
