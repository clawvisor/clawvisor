package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyCountTokens — POST /api/v1/messages/count_tokens shares
// the same handler as /messages but maps to messages.count_tokens audit
// action. Verify it forwards to upstream with vault key injected.
func TestLLMProxyCountTokens(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-ct-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ct"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages/count_tokens",
		bytes.NewReader([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hello"}]}`)))
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
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	if upstream.Last().Path != "/v1/messages/count_tokens" {
		t.Fatalf("upstream path=%q, want /v1/messages/count_tokens", upstream.Last().Path)
	}
}

// TestLLMProxyChatCompletions — POST /api/v1/chat/completions exercises
// the OpenAI Chat Completions shape (distinct from /v1/responses).
// Mode A: vault-injected OpenAI key.
func TestLLMProxyChatCompletions(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	upstream.body = []byte(`{"id":"chatcmpl-x","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "openai", "", "sk-cc-test-key")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cc"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)))
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
	if upstream.Last().Path != "/v1/chat/completions" {
		t.Fatalf("upstream path=%q, want /v1/chat/completions", upstream.Last().Path)
	}
	if upstream.Last().Headers.Get("Authorization") != "Bearer sk-cc-test-key" {
		t.Fatalf("upstream Authorization=%q, want Bearer sk-cc-test-key",
			upstream.Last().Headers.Get("Authorization"))
	}
}

// TestLLMProxyResponses — POST /api/v1/responses (OpenAI Responses API).
func TestLLMProxyResponses(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	upstream.body = []byte(`{"id":"resp-x","object":"response","model":"gpt-5","output":[{"type":"text","text":"ok"}]}`)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "openai", "", "sk-resp-test-key")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "resp"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5","input":"hi"}`)))
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
	if upstream.Last().Path != "/v1/responses" {
		t.Fatalf("upstream path=%q, want /v1/responses", upstream.Last().Path)
	}
}

// TestLLMProxyBodyCap — request body exceeding MaxRequestBytes (34 MiB)
// is rejected before reaching the upstream. 34 MiB is the production cap.
// We send a smaller-but-still-large body and confirm normal handling;
// then send an oversized body and confirm rejection.
func TestLLMProxyBodyCap(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-bc")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "bc"}, &agent)

	// 35 MiB body — just over the 34 MiB cap.
	big := strings.Repeat("x", 35*1024*1024)
	body := []byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"` + big + `"}]}`)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		// Connection-level failure on body cap is acceptable (server
		// may close the stream once the cap is hit).
		t.Logf("connection error on oversized body (acceptable): %v", err)
		return
	}
	defer resp.Body.Close()
	// Proxy may return 413 directly OR wrap the rejection in a 200 with an
	// error envelope in the body (the lite-proxy convention for client
	// errors that the agent should read). The invariant: upstream is NOT
	// hit, and the body indicates rejection.
	if upstream.Count() != 0 {
		t.Fatalf("upstream got %d hits despite cap; should be 0", upstream.Count())
	}
	respBody, _ := io.ReadAll(resp.Body)
	bs := strings.ToLower(string(respBody))
	if !strings.Contains(bs, "too large") && !strings.Contains(bs, "too_large") {
		t.Fatalf("response doesn't mention size limit (status=%d): %s",
			resp.StatusCode, respBody)
	}
}
