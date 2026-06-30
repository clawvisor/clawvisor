package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
	hllm "github.com/clawvisor/clawvisor/testharness/llm"
)

// TestLLMProxyEndToEndViaCassette runs the full path:
//
//   agent --POST /api/v1/messages--> clawvisor (proxy) --> cassette server --> upstream
//
// In replay mode, the cassette serves a pre-recorded Anthropic response;
// the upstream is never touched. In passthrough mode (used here when no
// cassette exists yet) it forwards to a stub upstream that produces an
// Anthropic-shaped response so the test is self-sufficient — no real API
// key required.
//
// The point of THIS test is end-to-end plumbing: clawvisor authenticates
// the agent, injects vault key, rewrites upstream URL, parses the upstream
// response, returns it to the agent. Use TestAnthropicLiveRecordReplay
// (in testharness/llm/) to validate the cassette layer against real Anthropic.
func TestLLMProxyEndToEndViaCassette(t *testing.T) {
	h := testharness.New(t)

	// Stub "Anthropic" that always returns a well-formed response.
	stubAnthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_test",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [{"type": "text", "text": "OK"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 5, "output_tokens": 1}
}`))
	}))
	defer stubAnthropic.Close()

	cassetteDir := filepath.Join("testdata", "llm-cassettes")
	mode := hllm.CurrentMode()
	// When no cassette exists and no ANTHROPIC_API_KEY, switch to passthrough
	// against the stub so the test still runs end-to-end without external deps.
	if mode == hllm.ModeReplay {
		matches, _ := filepath.Glob(filepath.Join(cassetteDir, t.Name(), "*.json"))
		if len(matches) == 0 {
			mode = hllm.ModePassthrough
		}
	}
	cassette := hllm.NewCassette(cassetteDir, t.Name(), mode)
	upstreamSrv := hllm.NewServer(t, cassette, stubAnthropic.URL)

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstreamSrv.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cassette-agent"}, &agent)

	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly the word OK."}]}`)
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

	// Structural assertion: the response has Anthropic shape.
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("parse: %v\nbody: %s", err, respBody)
	}
	if len(parsed.Content) == 0 || parsed.Content[0].Type != "text" {
		t.Fatalf("unexpected response shape: %s", respBody)
	}
	if parsed.Model == "" {
		t.Fatalf("missing model in response: %s", respBody)
	}
}
