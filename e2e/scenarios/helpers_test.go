// Package scenarios_test holds end-to-end scenario tests for
// clawvisor-server. Each file targets a feature area (LLM proxy,
// audit, scope drift, intent verification, etc.).
//
// Convention: every test boots a fresh clawvisor-server subprocess via
// testapp.Start, wires it to a testharness with in-process mocks for
// every external service, and talks to it over real HTTP. No in-process
// shortcuts — the surface matches what an agent would hit in
// production.
package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
)

// stubVerifier is a tiny Anthropic-compatible upstream that returns a
// scripted verifier verdict — used in place of a real Anthropic call.
// Shared by intent-verify and audit scenarios.
type stubVerifier struct {
	mu      sync.Mutex
	srv     *httptest.Server
	calls   int
	verdict string // JSON the verifier expects inside content[0].text
}

func newStubVerifier(t *testing.T, verdictJSON string) *stubVerifier {
	t.Helper()
	v := &stubVerifier{verdict: verdictJSON}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *stubVerifier) URL() string { return v.srv.URL }
func (v *stubVerifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func (v *stubVerifier) handle(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.calls++
	verdict := v.verdict
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	// Anthropic Messages API response shape — verifier reads
	// content[0].text and parses it as JSON.
	resp := map[string]any{
		"id":          "msg_verifier_stub",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-haiku-4-5-20251001",
		"content":     []map[string]any{{"type": "text", "text": verdict}},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// upstreamCapture is a tiny stand-in for api.anthropic.com /
// api.openai.com / etc. It captures every inbound request and answers
// with a configurable stub body so tests can assert on what
// clawvisor-server forwarded.
type upstreamCapture struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []capturedReq
	body     []byte
	status   int
	ct       string
}

type capturedReq struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

func newUpstreamCapture(t *testing.T) *upstreamCapture {
	t.Helper()
	u := &upstreamCapture{
		status: 200,
		ct:     "application/json",
		body:   []byte(`{"id":"msg_stub","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`),
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamCapture) URL() string { return u.srv.URL }

func (u *upstreamCapture) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	u.mu.Lock()
	u.requests = append(u.requests, capturedReq{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: r.Header.Clone(),
		Body:    body,
	})
	bodyOut, status, ct := u.body, u.status, u.ct
	u.mu.Unlock()
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = w.Write(bodyOut)
}

func (u *upstreamCapture) Last() capturedReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return capturedReq{}
	}
	return u.requests[len(u.requests)-1]
}

func (u *upstreamCapture) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// cvDo issues an HTTP request against clawvisor-server with the given
// access token. Body is JSON-marshalled if non-nil.
func cvDo(t *testing.T, cv *testapp.Server, tok, method, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cv.URL+path, r)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func cvPost(t *testing.T, cv *testapp.Server, tok, path string, body, dst any) {
	t.Helper()
	resp := cvDo(t, cv, tok, "POST", path, body)
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status=%d body=%s", path, resp.StatusCode, b)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
}

func cvGet(t *testing.T, cv *testapp.Server, tok, path string, dst any) {
	t.Helper()
	resp := cvDo(t, cv, tok, "GET", path, nil)
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s status=%d body=%s", path, resp.StatusCode, b)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
}

func cvPut(t *testing.T, cv *testapp.Server, tok, path string, body, dst any) {
	t.Helper()
	resp := cvDo(t, cv, tok, "PUT", path, body)
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s status=%d body=%s", path, resp.StatusCode, b)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
}

// readBodyStr is a tiny inline helper for failure-path printing —
// reads up to 4 KiB so we surface enough context without dragging in
// io.ReadAll at every call site.
func readBodyStr(resp *http.Response) string {
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

// llmCredSet stores an upstream LLM API key in the user's vault via the
// dedicated endpoint (the generic /api/vault/items rejects reserved
// provider IDs like "anthropic"). agentID="" means user-scope.
func llmCredSet(t *testing.T, cv *testapp.Server, userToken, provider, agentID, key string) {
	t.Helper()
	path := "/api/runtime/llm-credentials/" + provider
	if agentID != "" {
		path += "?agent_id=" + agentID
	}
	resp := cvDo(t, cv, userToken, "PUT", path, map[string]any{"api_key": key})
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s status=%d body=%s", path, resp.StatusCode, body)
	}
}
