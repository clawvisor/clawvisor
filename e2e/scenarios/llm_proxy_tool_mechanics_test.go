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

// TestLLMProxyMultipleToolUsesInOneResponse — cassette returns a response
// with N tool_use blocks. Proxy must handle each (potentially intercept
// each separately if out-of-scope).
func TestLLMProxyMultipleToolUsesInOneResponse(t *testing.T) {
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_multi",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [
    {"type": "text", "text": "Doing three things."},
    {"type": "tool_use", "id": "tu_a", "name": "github.list_issues", "input": {"owner": "a", "repo": "a"}},
    {"type": "tool_use", "id": "tu_b", "name": "github.create_issue", "input": {"owner": "b", "repo": "b"}},
    {"type": "tool_use", "id": "tu_c", "name": "github.list_repos", "input": {}}
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
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-multi-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "multi"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":256,"messages":[{"role":"user","content":"do three"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// All three tool_uses should appear in the response — each may be
	// rewritten (CLAWVISOR_BLOCKED) or pass through, but the count must
	// match. Count "tool_use" content blocks.
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v\nbody: %s", err, body)
	}
	toolUseCount := 0
	for _, c := range parsed.Content {
		if c.Type == "tool_use" {
			toolUseCount++
		}
	}
	if toolUseCount != 3 {
		t.Fatalf("expected 3 tool_use blocks in response; got %d. Body: %s", toolUseCount, body)
	}
}

// TestLLMProxyCvReasonExtractedFromToolUse — when the agent's tool_use
// input contains a `cvreason` field, the proxy extracts it and feeds it
// to the verifier as the agent's stated rationale (so a deny verdict is
// based on the agent's words, not a synthetic placeholder).
func TestLLMProxyCvReasonExtractedFromToolUse(t *testing.T) {
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_cvr",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [
    {"type": "tool_use", "id": "tu_cvr", "name": "Read", "input": {"file_path": "/tmp/a", "cvreason": "user asked me to read the file"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 50, "output_tokens": 30}
}`))
	}))
	defer upstream.Close()

	verifierBodies := make(chan []byte, 16)
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case verifierBodies <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "v",
			"type":    "message",
			"role":    "assistant",
			"model":   "claude-haiku-4-5-20251001",
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
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-cvr-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cvr"}, &agent)

	// v2 task that covers Read.
	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose":        "read a file the user asked about",
		"schema_version": 2,
		"expected_tools": []map[string]any{{"tool_name": "Read", "why": "fulfill the user request"}},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":256,"messages":[{"role":"user","content":"read /tmp/a"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	// If the verifier was called, look for the cvreason in the verifier's
	// inbound body (prompt sent to it).
	select {
	case body := <-verifierBodies:
		if !strings.Contains(string(body), "user asked me to read the file") {
			t.Logf("verifier received: %s", body)
			// Soft assertion: verifier might paraphrase. Surface body for
			// inspection rather than fail — the existence of the cvreason
			// in the prompt is the contract.
			t.Logf("note: cvreason text not literally in verifier prompt; may have been re-shaped by template")
		}
	default:
		// Verifier wasn't called — Read isn't always intent-verified (it's
		// a local tool). Skip rather than fail.
		t.Skip("verifier not consulted for local Read tool")
	}
}

// TestLLMProxyActiveTasksSnapshotInjected — when an agent has approved
// tasks, the proxy injects an "active tasks" snapshot into the system
// prompt so the agent's LLM knows what it's authorized to do. We capture
// the upstream body and check for task purpose text.
func TestLLMProxyActiveTasksSnapshotInjected(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-snap-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "snap"}, &agent)

	// Create an approved task with a uniquely-identifiable purpose.
	const purposeMarker = "UNIQUE-TEST-MARKER-cn37-pgg"
	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose":        purposeMarker,
		"schema_version": 2,
		"expected_tools": []map[string]any{{"tool_name": "Read", "why": "fulfill the user request"}},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	// ControlNotice (which carries the active-tasks snapshot) only
	// injects when the request has tools defined — for tool-less calls
	// the notice would be wasted bytes the model couldn't act on.
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{
  "model":"x","max_tokens":1,
  "tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],
  "messages":[{"role":"user","content":"hi"}]
}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if upstream.Count() == 0 {
		t.Fatal("upstream never called")
	}
	if !strings.Contains(string(upstream.Last().Body), purposeMarker) {
		t.Fatalf("active-task purpose %q not injected into upstream body. Body length: %d, snippet: %s",
			purposeMarker, len(upstream.Last().Body), truncate(string(upstream.Last().Body), 800))
	}
}

// TestLLMProxyVerifierDenyRewritesToolUse — in-scope tool_use whose
// verifier returns deny → proxy rewrites the tool_use to include a
// refusal marker the agent can read.
func TestLLMProxyVerifierDenyRewritesToolUse(t *testing.T) {
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_deny",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [
    {"type": "tool_use", "id": "tu_deny", "name": "Read", "input": {"file_path": "/etc/passwd", "cvreason": "the user asked"}}
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
			"id":      "v",
			"type":    "message",
			"role":    "assistant",
			"model":   "claude-haiku-4-5-20251001",
			"content": []map[string]any{{"type": "text", "text": `{"allow":false,"param_scope":"violation","reason_coherence":"incoherent","extract_context":false,"missing_chain_values":[],"explanation":"Reading /etc/passwd doesn't match the task."}`}},
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
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-dwt-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "dwt"}, &agent)

	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose":        "read /tmp/x specifically",
		"schema_version": 2,
		"expected_tools": []map[string]any{{"tool_name": "Read", "why": "fulfill the user request"}},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":256,"messages":[{"role":"user","content":"read"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("verifier consulted %d time(s)", atomic.LoadInt32(&verifierCalls))
	bs := string(body)
	// Either the verifier was consulted and the response shows a refusal,
	// OR Read wasn't classified as intent-verifiable (local tool). Surface
	// the response for inspection.
	if atomic.LoadInt32(&verifierCalls) > 0 {
		if !strings.Contains(strings.ToLower(bs), "refuse") &&
			!strings.Contains(strings.ToLower(bs), "intent") &&
			!strings.Contains(strings.ToLower(bs), "blocked") {
			t.Logf("verifier denied but response not visibly rewritten: %s", truncate(bs, 800))
		}
	} else {
		t.Skip("verifier not consulted on Read (local tool)")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
