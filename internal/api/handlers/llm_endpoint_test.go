package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type stubVault struct{ data map[string][]byte }

func (s *stubVault) Set(ctx context.Context, userID, serviceID string, c []byte) error {
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	s.data[userID+"/"+serviceID] = append([]byte{}, c...)
	return nil
}
func (s *stubVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	if v, ok := s.data[userID+"/"+serviceID]; ok {
		return append([]byte{}, v...), nil
	}
	return nil, vault.ErrNotFound
}
func (s *stubVault) Delete(ctx context.Context, userID, serviceID string) error { return nil }
func (s *stubVault) List(ctx context.Context, userID string) ([]string, error)  { return nil, nil }

func newSeededHandler(t *testing.T, upstreamURL string) (*LLMEndpointHandler, store.Store, string, string) {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "llm.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "lite-proxy@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rawAgentToken, err := auth.GenerateAgentToken()
	if err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "claude-code", auth.HashToken(rawAgentToken))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	v := &stubVault{}
	_ = v.Set(ctx, user.ID, "anthropic", []byte("sk-ant-real"))
	_ = v.Set(ctx, user.ID, "github", []byte("real-gh-token"))

	// Register a github placeholder so the rewrite-path boundary check
	// has something to bind against.
	placeholder := "autovault_github_xxx"
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      user.ID,
		AgentID:     agent.ID,
		ServiceID:   "github",
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}
	if err := st.CreateTask(ctx, &store.Task{
		UserID:  user.ID,
		AgentID: agent.ID,
		Purpose: "lite-proxy test github issue access",
		Status:  "active",
		AuthorizedActions: []store.TaskAction{{
			Service:      "github",
			Action:       "create_issue",
			Verification: "off",
		}},
		ExpectedEgress: json.RawMessage(`[{"host":"api.github.com","method":"POST","path":"/repos/x/y/issues","why":"test github issue access"}]`),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	h := NewLLMEndpointHandler(st, v, slog.Default())
	h.Forwarder = llmproxy.NewForwarder(v)
	h.Forwarder.Upstream = llmproxy.UpstreamSelector{
		AnthropicBaseURL: upstreamURL,
	}
	return h, st, rawAgentToken, placeholder
}

func TestLLMEndpoint_PassthroughAnthropic(t *testing.T) {
	var seenAPIKey, seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message"}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenAPIKey != "sk-ant-real" {
		t.Errorf("expected upstream x-api-key=sk-ant-real, got %q", seenAPIKey)
	}
	if seenPath != "/v1/messages" {
		t.Errorf("expected upstream /v1/messages, got %q", seenPath)
	}
	if string(seenBody) != string(body) {
		t.Errorf("body mismatch: %q vs %q", string(seenBody), string(body))
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if out["id"] != "msg_123" {
		t.Errorf("response did not pass through: %v", out)
	}
}

func TestLiteProxyDecisionPostureUsesAgentRuntimeMode(t *testing.T) {
	agent := &store.Agent{RuntimeSettings: &store.AgentRuntimeSettings{RuntimeMode: "observe"}}
	if got := liteProxyDecisionPosture(agent); got != "observe" {
		t.Fatalf("posture = %q, want observe", got)
	}
	agent.RuntimeSettings.RuntimeMode = "strict"
	if got := liteProxyDecisionPosture(agent); got != "enforce" {
		t.Fatalf("posture = %q, want enforce", got)
	}
}

func TestLLMEndpoint_AcceptsAnthropicXApiKey(t *testing.T) {
	var seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	// Anthropic SDK convention: agent token in x-api-key, not Authorization.
	body := []byte(`{"model":"claude","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("x-api-key", rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with x-api-key auth, got %d (%s)", rec.Code, rec.Body.String())
	}
	if seenAPIKey != "sk-ant-real" {
		t.Errorf("upstream should see vault key, not the agent token; got %q", seenAPIKey)
	}
}

func TestLLMEndpoint_VaultMissReturnsClearError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be hit when vault is empty")
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	// Override vault to empty stub.
	emptyVault := &stubVault{}
	h.Forwarder = llmproxy.NewForwarder(emptyVault)
	h.Forwarder.Upstream = llmproxy.UpstreamSelector{AnthropicBaseURL: upstream.URL}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 or 502 on vault miss, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestLLMEndpoint_RejectsMalformedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be hit on malformed body")
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("not-json"))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestLLMEndpoint_InspectorRewritesAutovaultToolUse(t *testing.T) {
	// Upstream returns an Anthropic response whose tool_use carries an
	// autovault_… placeholder in headers. The inspector's deterministic
	// parser classifies it as a credentialed call and rewrites the URL
	// to point at the resolver. Harness sees the rewritten URL.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",
			"content":[
				{"type":"text","text":"on it"},
				{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{
					"url":"https://api.github.com/repos/x/y/issues",
					"method":"POST",
					"headers":{"Authorization":"Bearer autovault_github_xxx"}
				}}
			],
			"stop_reason":"tool_use"
		}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.ResolverBaseURL = "https://clawvisor.example/proxy/v1"

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"create issue"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}

	for _, c := range resp.Content {
		if c.Type != "tool_use" {
			continue
		}
		var inputObj struct {
			URL     string         `json:"url"`
			Headers map[string]any `json:"headers"`
		}
		if err := json.Unmarshal(c.Input, &inputObj); err != nil {
			t.Fatalf("rewritten input not parseable: %v", err)
		}
		if !strings.HasPrefix(inputObj.URL, "https://clawvisor.example/proxy/v1/repos/x/y/issues") {
			t.Fatalf("URL not rewritten to resolver: %q", inputObj.URL)
		}
		if inputObj.Headers["X-Clawvisor-Target-Host"] != "api.github.com" {
			t.Fatalf("expected X-Clawvisor-Target-Host=api.github.com header, got %+v", inputObj.Headers)
		}
		if inputObj.Headers["Authorization"] != "Bearer autovault_github_xxx" {
			t.Fatalf("placeholder lost in rewrite: %+v", inputObj.Headers)
		}
	}
}

func TestLLMEndpoint_InspectorRewritesSSE(t *testing.T) {
	// Streaming version of the rewrite test: upstream returns SSE.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"url\":\"https://api.github.com/repos/x/y/issues\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer autovault_github_xxx\"}}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.ResolverBaseURL = "https://clawvisor.example/proxy/v1"

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	body := []byte(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"create issue"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "https://clawvisor.example/proxy/v1/repos/x/y/issues") {
		t.Fatalf("SSE response missing rewritten URL:\n%s", out)
	}
	if !strings.Contains(out, "X-Clawvisor-Target-Host") {
		t.Fatalf("SSE response missing X-Clawvisor-Target-Host:\n%s", out)
	}
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("SSE envelope missing:\n%s", out)
	}
}

func TestLLMEndpoint_InlineApprovalReleasesHeldToolUse(t *testing.T) {
	ctx := context.Background()
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id":"msg_1",
		  "type":"message",
		  "role":"assistant",
		  "content":[{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}}],
		  "stop_reason":"tool_use"
		}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.ResolverBaseURL = "https://clawvisor.example/proxy/v1"
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, slog.Default(), nil)
	agent, err := st.GetAgentByToken(ctx, auth.HashToken(rawToken))
	if err != nil {
		t.Fatalf("GetAgentByToken: %v", err)
	}
	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:       "review-webfetch",
		UserID:   agent.UserID,
		AgentID:  &agent.ID,
		Kind:     "tool",
		Action:   "review",
		ToolName: "WebFetch",
		Reason:   "review web fetch",
		Source:   "test",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	first := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"create issue"}]}`))
	first.Header.Set("Authorization", "Bearer "+rawToken)
	firstRec := httptest.NewRecorder()
	mux.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first response status = %d (%s)", firstRec.Code, firstRec.Body.String())
	}
	if !strings.Contains(firstRec.Body.String(), "Reply `approve`") {
		t.Fatalf("first response missing approval prompt: %s", firstRec.Body.String())
	}
	if upstreamHits != 1 {
		t.Fatalf("upstream hits after first request = %d, want 1", upstreamHits)
	}

	approve := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"approve"}]}`))
	approve.Header.Set("Authorization", "Bearer "+rawToken)
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approve)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("approve response status = %d (%s)", approveRec.Code, approveRec.Body.String())
	}
	if upstreamHits != 1 {
		t.Fatalf("approve should not call upstream, got hits=%d", upstreamHits)
	}
	out := approveRec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, "https://clawvisor.example/proxy/v1/repos/x/y/issues") {
		t.Fatalf("approve response did not release rewritten tool_use: %s", out)
	}
	entries, _, err := st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{AgentID: agent.ID, Limit: 20})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	foundRelease := false
	for _, entry := range entries {
		if entry.Action == "lite_proxy.approval.release" && entry.Outcome == "released" {
			foundRelease = true
			break
		}
	}
	if !foundRelease {
		t.Fatalf("missing approval release audit row: %+v", entries)
	}
}

// TestLLMEndpoint_EmitsAuditRow proves a /v1/* call writes an audit_log
// row that the dashboard picks up — visibility into "what did my agents
// do via lite-proxy" is the trust feature gating production use.
func TestLLMEndpoint_EmitsAuditRow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, nil, nil)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	body := []byte(`{"model":"claude-haiku-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Pull the agent's user_id to scope the audit query.
	user, _ := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	rows, _, err := st.ListAuditEntries(context.Background(), user.ID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected audit rows, got none")
	}
	var found bool
	for _, row := range rows {
		if row.Action == "lite_proxy.messages.create" && row.Decision == "allow" && row.Outcome == "success" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected lite_proxy.messages.create audit row; got %d rows", len(rows))
	}
}

func TestLLMEndpoint_RejectsMissingAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be hit when auth missing")
	}))
	defer upstream.Close()

	h, st, _, _ := newSeededHandler(t, upstream.URL)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
