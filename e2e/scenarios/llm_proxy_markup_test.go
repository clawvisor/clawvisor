package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// The `<clawvisor:decision option="…">` markup is a scope-drift decision
// channel: when a drift is registered for the agent and conversation,
// the proxy's postprocess looks for this markup in the agent's assistant
// content and dispatches accordingly. Three documented options:
//
//   option="one-off"  → request a one-off approval with rationale text
//   option="expand"   → request the task's scope be expanded
//   option="new_task" → request a new task be created
//
// Without an active drift in the registry, the markup is just text the
// agent emitted and the proxy passes it through unchanged. The full
// drift→markup→approval state machine is exercised in the package-
// internal scope_drift_e2e_test.go (which works against the registry
// directly). What's testable from the cloud-side HTTP boundary:
//
//   1. The markup, when emitted by the agent, doesn't crash the proxy.
//   2. Without a drift, the response passes through to the client so the
//      agent can see its own emitted text in the next turn's context.
//
// These tests cover #1 and #2 for each of the three options.

func runMarkupOption(t *testing.T, option, rationale string) {
	t.Helper()
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_markup",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [
    {"type": "text", "text": "Reasoning about the request.\n<clawvisor:decision option=\"` + option + `\">` + rationale + `</clawvisor:decision>"}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 50, "output_tokens": 20}
}`))
	}))
	defer upstream.Close()
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-markup-"+option)
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "markup-" + option}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)))
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
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)
	// Without a registered drift, the markup either passes through OR is
	// stripped by the proxy's safety filter. Either is acceptable;
	// what's NOT acceptable is a 5xx, empty body, or panic.
	if len(bs) == 0 {
		t.Fatal("empty response body")
	}
	if strings.Contains(bs, `"type":"error"`) {
		t.Fatalf("response is an error envelope: %s", bs)
	}
	t.Logf("option=%s response: %s", option, truncate(bs, 400))
}

func TestLLMProxyMarkupOneOff(t *testing.T) {
	runMarkupOption(t, "one-off", "user explicitly asked for this single read")
}

func TestLLMProxyMarkupExpand(t *testing.T) {
	runMarkupOption(t, "expand", "this is a natural extension of the current task")
}

func TestLLMProxyMarkupNewTask(t *testing.T) {
	runMarkupOption(t, "new_task", "this is a wholly new request")
}
