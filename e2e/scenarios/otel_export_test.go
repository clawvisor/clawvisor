package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
	hllm "github.com/clawvisor/clawvisor/testharness/llm"
	"github.com/clawvisor/clawvisor/testharness/otlp"
)

// otelEnv returns the env overrides that point the clawvisor-server
// subprocess's OTel exporter at the in-process receiver, with a short metric
// interval so the deterministic lane doesn't wait a full minute.
func otelEnv(rcv *otlp.Receiver, extra map[string]string) map[string]string {
	env := map[string]string{
		"CLAWVISOR_OTEL_ENABLED":                  "true",
		"CLAWVISOR_OTEL_ENDPOINT":                 rcv.Endpoint(),
		"CLAWVISOR_OTEL_PROTOCOL":                 "http",
		"CLAWVISOR_OTEL_INSECURE":                 "true",
		"CLAWVISOR_OTEL_METRICS_INTERVAL_SECONDS": "1",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// stubAnthropicWithEcho returns a stub upstream that echoes the sentinel
// nowhere but returns a well-formed Anthropic response carrying usage so the
// token/cost counters fire.
func stubAnthropic(t *testing.T, reply string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "msg_test",
  "type": "message",
  "role": "assistant",
  "model": "claude-haiku-4-5-20251001",
  "content": [{"type": "text", "text": "` + reply + `"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 5, "output_tokens": 1}
}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bootProxyLiteWithOTel wires a proxy-lite server against a stub Anthropic
// upstream (via cassette passthrough fallback) with OTel export enabled.
func bootProxyLiteWithOTel(t *testing.T, rcv *otlp.Receiver, stubURL string) (*testapp.Server, string) {
	t.Helper()
	h := testharness.New(t)

	cassetteDir := filepath.Join("testdata", "llm-cassettes")
	mode := hllm.CurrentMode()
	if mode == hllm.ModeReplay {
		matches, _ := filepath.Glob(filepath.Join(cassetteDir, t.Name(), "*.json"))
		if len(matches) == 0 {
			mode = hllm.ModePassthrough
		}
	}
	cassette := hllm.NewCassette(cassetteDir, t.Name(), mode)
	upstreamSrv := hllm.NewServer(t, cassette, stubURL)

	cv := testapp.StartWith(t, h, otelEnv(rcv, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstreamSrv.URL(),
	}))
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "otel-agent"}, &agent)
	return cv, agent.Token
}

func postMessages(t *testing.T, cv *testapp.Server, agentToken string, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	return resp
}

// TestProxyLiteRequestEmitsSpanTree boots proxy-lite with OTel enabled
// pointing at an in-process OTLP receiver, runs one LLM request, and asserts
// the span tree (proxy_lite.request → pipeline.pre, upstream.forward,
// pipeline.post) plus the llm.requests / llm.tokens / llm.cost.usd_micros
// counters.
func TestProxyLiteRequestEmitsSpanTree(t *testing.T) {
	rcv := otlp.NewReceiver(t)
	stub := stubAnthropic(t, "OK")
	cv, agentToken := bootProxyLiteWithOTel(t, rcv, stub.URL)

	resp := postMessages(t, cv, agentToken,
		`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly the word OK."}]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	root := rcv.WaitForSpan("clawvisor.proxy_lite.request", 25*time.Second)
	if root == nil {
		t.Fatal("root span clawvisor.proxy_lite.request not exported")
	}
	// Assert the three phase children exist under the root.
	for _, name := range []string{
		"clawvisor.pipeline.pre",
		"clawvisor.upstream.forward",
		"clawvisor.pipeline.post",
	} {
		child := rcv.WaitForSpan(name, 25*time.Second)
		if child == nil {
			t.Fatalf("child span %s not exported", name)
		}
		if !bytes.Equal(child.GetTraceId(), root.GetTraceId()) {
			t.Errorf("child %s not in root trace: child trace=%x root trace=%x", name, child.GetTraceId(), root.GetTraceId())
		}
		if !bytes.Equal(child.GetParentSpanId(), root.GetSpanId()) {
			t.Errorf("child %s parent=%x, want root span=%x", name, child.GetParentSpanId(), root.GetSpanId())
		}
	}

	// The request-side policy chain runs before the upstream forward, so its
	// per-policy policy.verdict events must attach to the pipeline.pre span —
	// not the root. (Regression guard: the pre phase span used to be opened
	// only after the pre-policy chain had already run, so every verdict event
	// landed on the root span.)
	preSpan := rcv.SpanByName("clawvisor.pipeline.pre")
	if preSpan == nil {
		t.Fatal("pipeline.pre span not exported")
	}
	preVerdicts := 0
	for _, ev := range preSpan.GetEvents() {
		if ev.GetName() == "policy.verdict" {
			preVerdicts++
		}
	}
	if preVerdicts == 0 {
		t.Error("pipeline.pre span carries no policy.verdict events; pre-policy verdicts are not attaching to the pre phase span")
	}
	for _, ev := range root.GetEvents() {
		if ev.GetName() == "policy.verdict" {
			t.Errorf("root span carries a policy.verdict event %q; pre-policy verdicts must attach to pipeline.pre, not root", ev.GetName())
		}
	}

	// Root span attributes.
	if got := otlp.AttrString(root.GetAttributes(), "provider"); got != "anthropic" {
		t.Errorf("root span provider=%q, want anthropic", got)
	}
	if got := otlp.AttrString(root.GetAttributes(), "clawvisor.outcome"); got != "allowed" {
		t.Errorf("root span clawvisor.outcome=%q, want allowed", got)
	}
	if !otlp.HasAttr(root.GetAttributes(), "clawvisor.agent_id") {
		t.Error("root span missing clawvisor.agent_id")
	}

	// llm.requests counter.
	reqMetric := rcv.WaitForMetric("clawvisor.llm.requests", 25*time.Second)
	if reqMetric == nil {
		t.Fatal("clawvisor.llm.requests not exported")
	}
	if got := otlp.SumForAttrs(reqMetric, map[string]string{
		"provider": "anthropic", "outcome": "allowed", "auth_mode": "vault",
	}); got < 1 {
		t.Errorf("llm.requests{provider=anthropic,outcome=allowed,auth_mode=vault}=%d, want >=1", got)
	}

	// llm.tokens counter agrees with the stub usage (input=5, output=1).
	tokMetric := rcv.WaitForMetric("clawvisor.llm.tokens", 25*time.Second)
	if tokMetric == nil {
		t.Fatal("clawvisor.llm.tokens not exported")
	}
	if got := otlp.SumForAttrs(tokMetric, map[string]string{"direction": "input"}); got != 5 {
		t.Errorf("llm.tokens{direction=input}=%d, want 5", got)
	}
	if got := otlp.SumForAttrs(tokMetric, map[string]string{"direction": "output"}); got != 1 {
		t.Errorf("llm.tokens{direction=output}=%d, want 1", got)
	}

	// cost counter is emitted at the same point; present when the model is
	// priced. Assert it exported and is positive when a data point exists.
	if costMetric := rcv.WaitForMetric("clawvisor.llm.cost.usd_micros", 10*time.Second); costMetric != nil {
		if got := otlp.SumForAttrs(costMetric, map[string]string{"provider": "anthropic"}); got < 0 {
			t.Errorf("llm.cost.usd_micros negative: %d", got)
		}
	}

	// pipeline.verdicts counter is emitted for the request-side policy chain.
	if vMetric := rcv.WaitForMetric("clawvisor.pipeline.verdicts", 10*time.Second); vMetric != nil {
		if len(otlp.CounterAttrValues(vMetric, "phase")) == 0 {
			t.Error("pipeline.verdicts exported with no phase attribute")
		}
	}
}

// TestDeniedRequestVerdictMetric drives a request that is denied at the
// proxy-lite layer (no upstream credential) and asserts the deny surfaces on
// the root span (clawvisor.outcome=denied) and the llm.requests counter.
func TestDeniedRequestVerdictMetric(t *testing.T) {
	rcv := otlp.NewReceiver(t)
	stub := stubAnthropic(t, "OK")
	h := testharness.New(t)

	cassetteDir := filepath.Join("testdata", "llm-cassettes")
	cassette := hllm.NewCassette(cassetteDir, t.Name(), hllm.ModePassthrough)
	upstreamSrv := hllm.NewServer(t, cassette, stub.URL)

	cv := testapp.StartWith(t, h, otelEnv(rcv, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstreamSrv.URL(),
	}))
	user := cv.LoginAsLocalUser(t)
	// Deliberately do NOT set an upstream credential → vault miss → deny.
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "denied-agent"}, &agent)

	resp := postMessages(t, cv, agent.Token,
		`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	root := rcv.WaitForSpan("clawvisor.proxy_lite.request", 25*time.Second)
	if root == nil {
		t.Fatal("root span not exported")
	}
	if got := otlp.AttrString(root.GetAttributes(), "clawvisor.outcome"); got != "denied" {
		t.Errorf("root span clawvisor.outcome=%q, want denied", got)
	}

	reqMetric := rcv.WaitForMetric("clawvisor.llm.requests", 25*time.Second)
	if reqMetric == nil {
		t.Fatal("clawvisor.llm.requests not exported")
	}
	if got := otlp.SumForAttrs(reqMetric, map[string]string{"outcome": "denied"}); got < 1 {
		t.Errorf("llm.requests{outcome=denied}=%d, want >=1", got)
	}
}

// TestNoContentInAttributes runs a request whose prompt contains a sentinel
// string and asserts the sentinel appears in NO exported span or metric
// attribute.
func TestNoContentInAttributes(t *testing.T) {
	const sentinel = "SENTINEL_SECRET_PROMPT_XYZZY_9182"
	rcv := otlp.NewReceiver(t)
	stub := stubAnthropic(t, "OK")
	cv, agentToken := bootProxyLiteWithOTel(t, rcv, stub.URL)

	resp := postMessages(t, cv, agentToken,
		`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"`+sentinel+`"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Wait until the span tree has exported so there is telemetry to inspect.
	if rcv.WaitForSpan("clawvisor.proxy_lite.request", 25*time.Second) == nil {
		t.Fatal("root span not exported; cannot assert content safety")
	}
	rcv.WaitForMetric("clawvisor.llm.requests", 25*time.Second)

	for _, v := range rcv.AllStringAttrValues() {
		if strings.Contains(v, sentinel) {
			t.Fatalf("sentinel prompt content leaked into telemetry attribute: %q", v)
		}
	}
}

// TestGatewayRequestMetric issues one gateway call and asserts the
// clawvisor.gateway.requests counter records the alias-stripped service.
func TestGatewayRequestMetric(t *testing.T) {
	rcv := otlp.NewReceiver(t)
	h := testharness.New(t)
	cv := testapp.StartWith(t, h, otelEnv(rcv, nil))

	user := cv.LoginAsLocalUser(t)
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "gw-agent"}, &agent)

	// A gateway request with an alias-qualified service. It need not
	// succeed downstream — clawvisor.gateway.requests is emitted for every
	// request that reaches the handler, with the alias stripped.
	req, _ := http.NewRequest("POST", cv.URL+"/api/gateway/request",
		bytes.NewReader([]byte(`{"service":"google.gmail:work","action":"list_messages","reason":"otel gateway metric test","request_id":"otel-gw-1"}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	m := rcv.WaitForMetric("clawvisor.gateway.requests", 25*time.Second)
	if m == nil {
		t.Fatal("clawvisor.gateway.requests not exported")
	}
	services := otlp.CounterAttrValues(m, "service")
	if !services["google.gmail"] {
		t.Errorf("gateway.requests service values=%v, want alias-stripped google.gmail", services)
	}
}
