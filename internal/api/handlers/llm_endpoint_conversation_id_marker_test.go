package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestLLMEndpoint_ChatCompletions_FirstTurnMintsConversationIDMarker
// exercises the end-to-end inject path: turn 1 of a Chat Completions
// conversation mints a Clawvisor-owned conversation ID, embeds the
// marker in the response routing notice, and records the mint in the
// audit row.
func TestLLMEndpoint_ChatCompletions_FirstTurnMintsConversationIDMarker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Forwarder.Upstream.OpenAIBaseURL = upstream.URL
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, nil, nil)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/chat/completions", mw(http.HandlerFunc(h.ChatCompletions)))

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Routing notice + marker land in the assistant content of the
	// first choice. PrependOpenAIChatAssistantText inserts the notice
	// at the head of the existing content string.
	respText := rec.Body.String()
	if !strings.Contains(respText, "Routing this conversation through Clawvisor") {
		t.Errorf("response missing routing notice: %s", respText)
	}
	if !strings.Contains(respText, conversation.ConversationIDMarker+conversation.ConversationIDPrefix) {
		t.Errorf("response missing conversation ID marker: %s", respText)
	}

	// Pull the marker back out and verify the scanner round-trips it.
	// Build a synthetic turn-2 body where the assistant turn is exactly
	// the response we just got, and assert FindInjectedConversationID
	// recovers the same value.
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("response has no choices")
	}
	assistantContent := parsed.Choices[0].Message.Content
	turn2Body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":` + mustJSONStr(t, assistantContent) + `},{"role":"user","content":"follow up"}]}`)
	turn2Req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recoveredID := conversation.FindInjectedConversationID(turn2Req, conversation.ProviderOpenAI, turn2Body)
	if recoveredID == "" {
		t.Fatalf("marker did not round-trip: assistant=%q", assistantContent)
	}

	// Audit row should record the mint + first_turn + source=minted.
	user, _ := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	rows, _, err := st.ListAuditEntries(context.Background(), user.ID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	var serveRow *store.AuditEntry
	for _, row := range rows {
		if row.Action == "lite_proxy.chat.completions.create" {
			serveRow = row
			break
		}
	}
	if serveRow == nil {
		t.Fatalf("expected chat.completions.create audit row; got %d rows", len(rows))
	}
	var params map[string]any
	if err := json.Unmarshal(serveRow.ParamsSafe, &params); err != nil {
		t.Fatalf("parse params_safe: %v", err)
	}
	if params["first_turn"] != true {
		t.Errorf("expected first_turn=true, params=%v", params)
	}
	if params["conversation_id_minted"] != true {
		t.Errorf("expected conversation_id_minted=true, params=%v", params)
	}
	if params["conversation_id_source"] != "minted" {
		t.Errorf("expected conversation_id_source=minted, got %v", params["conversation_id_source"])
	}
	if cid, _ := params["conversation_id"].(string); cid != recoveredID {
		t.Errorf("expected audit conversation_id=%q to match recovered marker %q", cid, recoveredID)
	}
}

// TestLLMEndpoint_ChatCompletions_Turn2EchoesMarkerNoRemint confirms a
// continuation request that carries the marker in assistant history
// re-uses the same conversation ID without minting a new one and
// without re-prepending the routing notice.
func TestLLMEndpoint_ChatCompletions_Turn2EchoesMarkerNoRemint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl_2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Forwarder.Upstream.OpenAIBaseURL = upstream.URL
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, nil, nil)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/chat/completions", mw(http.HandlerFunc(h.ChatCompletions)))

	const echoedID = "cv-conv-abcdefghijklmnopqrstuvwxyz"
	body := []byte(`{"model":"gpt-4o","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"[Clawvisor] Routing this conversation through Clawvisor. [clawvisor:conversation=` + echoedID + `]"},
		{"role":"user","content":"follow up"}
	]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	// Turn 2: no notice, no new marker — assistant content stays as
	// the upstream's "ok" verbatim. (Note: marker MAY appear later in
	// the body since fluentd-style mock JSON could contain literal
	// strings; the test below checks the specific assistant content.)
	respBody, _ := io.ReadAll(rec.Body)
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content != "ok" {
		t.Fatalf("expected assistant content unchanged (\"ok\"), got %+v", parsed.Choices)
	}

	user, _ := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	rows, _, err := st.ListAuditEntries(context.Background(), user.ID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	var serveRow *store.AuditEntry
	for _, row := range rows {
		if row.Action == "lite_proxy.chat.completions.create" {
			serveRow = row
			break
		}
	}
	if serveRow == nil {
		t.Fatalf("expected chat.completions.create audit row; got %d rows", len(rows))
	}
	var params map[string]any
	if err := json.Unmarshal(serveRow.ParamsSafe, &params); err != nil {
		t.Fatalf("parse params_safe: %v", err)
	}
	if params["first_turn"] != false {
		t.Errorf("expected first_turn=false, params=%v", params)
	}
	if _, minted := params["conversation_id_minted"]; minted {
		t.Errorf("expected no conversation_id_minted key on turn 2, params=%v", params)
	}
	if params["conversation_id_source"] != "echoed_marker" {
		t.Errorf("expected conversation_id_source=echoed_marker, got %v", params["conversation_id_source"])
	}
	if cid, _ := params["conversation_id"].(string); cid != echoedID {
		t.Errorf("expected audit conversation_id=%q to equal echoed marker", cid)
	}
}

// TestLLMEndpoint_Anthropic_MintsMarkerWhenNoMetadata pins the
// cross-provider marker mint: an Anthropic /v1/messages request with
// no metadata.user_id (raw API client, not Claude Code) lands on the
// fingerprint path, the handler detects that and mints a cv-conv-
// marker, and the marker is prepended to the first assistant response.
// The marker is what makes Anthropic raw-API conversations compaction-
// tolerant — the harness echoes it back on subsequent turns and
// FindInjectedConversationID recovers it.
func TestLLMEndpoint_Anthropic_MintsMarkerWhenNoMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"claude-sonnet-4","stop_reason":"end_turn"}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, nil, nil)

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
	respText := rec.Body.String()
	if !strings.Contains(respText, "Routing this conversation through Clawvisor") {
		t.Errorf("Anthropic response missing routing notice: %s", respText)
	}
	if !strings.Contains(respText, conversation.ConversationIDMarker) {
		t.Errorf("Anthropic response missing conversation ID marker (no metadata + no echo → mint MUST fire): %s", respText)
	}

	user, _ := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	rows, _, err := st.ListAuditEntries(context.Background(), user.ID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	for _, row := range rows {
		if row.Action != "lite_proxy.messages.create" {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal(row.ParamsSafe, &params)
		if minted, _ := params["conversation_id_minted"].(bool); !minted {
			t.Errorf("Anthropic turn must mint a conversation ID when no native session_id is present: params=%v", params)
		}
		src, _ := params["conversation_id_source"].(string)
		if src != "minted" {
			t.Errorf("conversation_id_source = %q, want \"minted\"", src)
		}
		convID, _ := params["conversation_id"].(string)
		if !strings.HasPrefix(convID, conversation.ConversationIDPrefix) {
			t.Errorf("minted conversation_id = %q, want cv-conv- prefix", convID)
		}
		return
	}
	t.Fatalf("expected messages.create audit row")
}

// TestLLMEndpoint_Anthropic_NativeSessionSkipsMint confirms the
// inverse: when Anthropic clients DO set metadata.user_id.session_id
// (Claude Code's convention), the handler uses the native id and
// skips minting. The native path is the dominant Anthropic
// population — minting would just be wasted entropy.
func TestLLMEndpoint_Anthropic_NativeSessionSkipsMint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"claude-sonnet-4","stop_reason":"end_turn"}`))
	}))
	defer upstream.Close()

	h, st, rawToken, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.AuditEmitter = llmproxy.NewAuditEmitter(st, nil, nil)

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLM(st)
	mux.Handle("POST /v1/messages", mw(http.HandlerFunc(h.Messages)))

	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"{\"session_id\":\"sess-native-claude-code\"}"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	respText := rec.Body.String()
	if strings.Contains(respText, conversation.ConversationIDMarker) {
		t.Errorf("Anthropic response with native session_id must NOT carry minted marker: %s", respText)
	}

	user, _ := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	rows, _, err := st.ListAuditEntries(context.Background(), user.ID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	for _, row := range rows {
		if row.Action != "lite_proxy.messages.create" {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal(row.ParamsSafe, &params)
		if minted, _ := params["conversation_id_minted"].(bool); minted {
			t.Errorf("Anthropic native-session turn must NOT mint: params=%v", params)
		}
		src, _ := params["conversation_id_source"].(string)
		if src != "native_anthropic" {
			t.Errorf("conversation_id_source = %q, want \"native_anthropic\"", src)
		}
		return
	}
	t.Fatalf("expected messages.create audit row")
}

func mustJSONStr(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
