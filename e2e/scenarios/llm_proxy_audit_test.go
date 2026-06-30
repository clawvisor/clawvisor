package scenarios_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyEmitsAuditPerRequest — every /api/v1/* request must
// produce a row in the audit log with the matching action label
// (messages.create / chat.completions.create / responses.create).
// We send three distinct requests then read /api/audit and assert
// each action appears.
func TestLLMProxyEmitsAuditPerRequest(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	upstream.body = []byte(`{"id":"x","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
		"CLAWVISOR_LLM_UPSTREAM_OPENAI":    upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-test")
	llmCredSet(t, cv, user.AccessToken, "openai", "", "sk-audit-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit"}, &agent)

	doRequest := func(path, body string) {
		req, _ := http.NewRequest("POST", cv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+agent.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := cv.Client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
	}
	doRequest("/api/v1/messages", `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"a"}]}`)
	doRequest("/api/v1/messages/count_tokens", `{"model":"x","messages":[{"role":"user","content":"a"}]}`)
	doRequest("/api/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"b"}]}`)

	// Give the audit writer a moment to flush.
	time.Sleep(150 * time.Millisecond)

	// Read the audit log (top-level shape varies by config — accept any).
	resp, err := http.NewRequest("GET", cv.URL+"/api/audit", nil)
	_ = err
	resp.Header.Set("Authorization", "Bearer "+user.AccessToken)
	httpResp, err := cv.Client.Do(resp)
	if err != nil {
		t.Fatalf("audit get: %v", err)
	}
	body := readBodyStr(httpResp)
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", httpResp.StatusCode, body)
	}
	// All three action labels should appear.
	for _, want := range []string{"messages.create", "messages.count_tokens", "chat.completions.create"} {
		if !strings.Contains(body, want) {
			t.Fatalf("audit log missing %q action; body: %s", want, truncate(body, 1200))
		}
	}
}
