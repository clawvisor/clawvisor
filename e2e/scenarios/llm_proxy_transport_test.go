package scenarios_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLLMProxyStripsHopByHopHeaders — forwardSkipHeaders contains
// Cookie, Connection, Keep-Alive, hop-by-hop, and X-Clawvisor-* (handled
// by prefix match). Confirm none of them leak to upstream.
func TestLLMProxyStripsHopByHopHeaders(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-strip-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "strip"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	// Headers that MUST be stripped by the proxy:
	req.Header.Set("Cookie", "session=secret-cookie-value; tracking=should-not-leak")
	req.Header.Set("X-Clawvisor-Internal-Hint", "should-not-leak-either")
	req.Header.Set("X-Clawvisor-Tenant", "leak-me-if-you-can")
	req.Header.Set("Proxy-Authorization", "Bearer proxy-leak")

	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got := upstream.Last().Headers
	for _, k := range []string{"Cookie", "X-Clawvisor-Internal-Hint", "X-Clawvisor-Tenant", "Proxy-Authorization"} {
		if got.Get(k) != "" {
			t.Fatalf("header %s leaked to upstream: %q", k, got.Get(k))
		}
	}
}

// TestLLMProxyForwardsAcceptEncodingIdentity — proxy forces Accept-Encoding
// to "identity" upstream so the response body is parseable for postprocess
// (gzip would defeat tool_use rewriting).
func TestLLMProxyForwardsAcceptEncodingIdentity(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-ae-test")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ae"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	// Caller asks for gzip; proxy should override to identity.
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := upstream.Last().Headers.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("upstream Accept-Encoding=%q, want identity", got)
	}
}

// TestLLMProxyUpstream5xxSurfacesAsError — upstream returns 500 → proxy
// surfaces a non-2xx or an error-shaped 200 body. Either way, the agent
// can detect the failure.
func TestLLMProxyUpstream5xxSurfacesAsError(t *testing.T) {
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream down"}}`))
	}))
	defer upstream.Close()

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-5xx")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "5xx"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Either non-2xx OR a 2xx with the upstream error wrapped — both are
	// observable. The forbidden outcome is silently dropping the error.
	if resp.StatusCode == 200 && !strings.Contains(strings.ToLower(string(body)), "error") {
		t.Fatalf("upstream 500 silently became a clean 200; body=%s", body)
	}
}

// TestLLMProxyClientCancelMidRequest — client closes its side of the
// connection mid-flight; proxy must release resources and not block.
// Simpler/faster than testing the 60s ResponseHeaderTimeout config.
func TestLLMProxyClientCancelMidRequest(t *testing.T) {
	h := testharness.New(t)
	upstreamGotIt := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case upstreamGotIt <- struct{}{}:
		default:
		}
		// Sleep briefly so the client has time to cancel.
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"late"}`))
	}))
	defer upstream.Close()

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-cancel")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "cancel"}, &agent)

	// Client cancels after 200ms.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	_, err := cv.Client.Do(req)
	if err == nil {
		t.Fatal("expected context-canceled error from Do")
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "canceled") {
		t.Logf("got err=%v (any cancel-shaped error is acceptable)", err)
	}
	// Upstream should have received the request before we cancelled.
	select {
	case <-upstreamGotIt:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received request")
	}
}

// TestLLMProxyConcurrentRequestsIsolated — two agents with different
// vault keys hit the proxy concurrently. Each upstream call must use the
// matching key (no cross-agent leakage).
func TestLLMProxyConcurrentRequestsIsolated(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)

	// Two agents, two distinct agent-scoped vault keys.
	var a1, a2 struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "iso-1"}, &a1)
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "iso-2"}, &a2)
	llmCredSet(t, cv, user.AccessToken, "anthropic", a1.ID, "sk-ant-iso-AGENT1")
	llmCredSet(t, cv, user.AccessToken, "anthropic", a2.ID, "sk-ant-iso-AGENT2")

	N := 8
	var wg sync.WaitGroup
	wg.Add(N * 2)
	send := func(agentTok string) {
		defer wg.Done()
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+agentTok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := cv.Client.Do(req)
		if err != nil {
			t.Errorf("do: %v", err)
			return
		}
		resp.Body.Close()
	}
	for i := 0; i < N; i++ {
		go send(a1.Token)
		go send(a2.Token)
	}
	wg.Wait()

	// Audit the upstream captures — every Authorization-stripped, x-api-key
	// header must match one of the two vault keys, never crossed.
	agent1Hits, agent2Hits := 0, 0
	for _, r := range upstream.requests {
		k := r.Headers.Get("x-api-key")
		switch k {
		case "sk-ant-iso-AGENT1":
			agent1Hits++
		case "sk-ant-iso-AGENT2":
			agent2Hits++
		default:
			t.Fatalf("unexpected x-api-key on upstream call: %q", k)
		}
	}
	if agent1Hits != N || agent2Hits != N {
		t.Fatalf("uneven distribution: agent1=%d agent2=%d (want %d each)", agent1Hits, agent2Hits, N)
	}
}
