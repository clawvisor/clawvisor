package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/store/sqlite"
)

func TestInjectResponseNoticeIntoAnthropicJSON(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	notice := observeModeInjectedUserNotice("agent_123", "http://127.0.0.1:25297")

	rewritten, changed := injectResponseNoticesBody(req, "application/json", body, []responseNotice{{Kind: "observe_mode", Text: notice}})
	if !changed {
		t.Fatal("expected anthropic response to be rewritten")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	content, _ := payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	first, _ := content[0].(map[string]any)
	if got, _ := first["text"].(string); got != prefixNoticeText(notice, "hello") {
		t.Fatalf("expected prefixed notice, got %q", got)
	}
}

func TestInjectResponseNoticeIntoOpenAIChatJSON(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	body := []byte(`{"id":"chatcmpl_1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`)
	notice := observeModeInjectedUserNotice("agent_123", "http://127.0.0.1:25297")

	rewritten, changed := injectResponseNoticesBody(req, "application/json", body, []responseNotice{{Kind: "observe_mode", Text: notice}})
	if !changed {
		t.Fatal("expected openai chat response to be rewritten")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	choices, _ := payload["choices"].([]any)
	first, _ := choices[0].(map[string]any)
	message, _ := first["message"].(map[string]any)
	content, _ := message["content"].(string)
	if content != prefixNoticeText(notice, "hello") {
		t.Fatalf("expected content to be prefixed with observe notice, got %q", content)
	}
}

func TestScrubHistoricalResponseNoticesFromAnthropicRequest(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	body := []byte(`{
	  "messages": [
	    {"role":"assistant","content":[{"type":"text","text":"👍"},{"type":"text","text":"Clawvisor is in observe mode. Actions are being analyzed and logged, but not blocked."}]},
	    {"role":"assistant","content":[{"type":"text","text":"([Clawvisor system message]: Clawvisor is currently running in observe mode. Actions are being analyzed and logged, but not blocked. Change this in Clawvisor: http://127.0.0.1:25297/dashboard/agents/agent_123)\n\nStill going 😄."}]},
	    {"role":"user","content":[{"type":"text","text":"Please keep the quoted text: ([Clawvisor system message]: Clawvisor is currently running in observe mode.)"}]}
	  ]
	}`)

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected anthropic request history to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	firstContent, _ := first["content"].([]any)
	if len(firstContent) != 1 {
		t.Fatalf("expected standalone observe notice block to be removed, got %d blocks", len(firstContent))
	}
	second, _ := messages[1].(map[string]any)
	secondContent, _ := second["content"].([]any)
	block, _ := secondContent[0].(map[string]any)
	if got, _ := block["text"].(string); got != "Still going 😄." {
		t.Fatalf("expected prefixed observe notice to be removed, got %q", got)
	}
	third, _ := messages[2].(map[string]any)
	thirdContent, _ := third["content"].([]any)
	userBlock, _ := thirdContent[0].(map[string]any)
	if got, _ := userBlock["text"].(string); !strings.Contains(got, "Clawvisor system message") {
		t.Fatalf("expected user-authored quoted text to be preserved, got %q", got)
	}
}

func TestScrubHistoricalResponseNoticesFromOpenAIChatRequest(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	body := []byte(`{
	  "messages": [
	    {"role":"assistant","content":"([Clawvisor system message]: Clawvisor is currently running in observe mode. Actions are being analyzed and logged, but not blocked.)\n\nHello"},
	    {"role":"assistant","content":[{"type":"text","text":"(Clawvisor is in observe mode. Actions are being analyzed and logged, but not blocked.)\n\nHi again"}]}
	  ]
	}`)

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected openai chat request history to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if got, _ := first["content"].(string); got != "Hello" {
		t.Fatalf("expected string content to be scrubbed, got %q", got)
	}
	second, _ := messages[1].(map[string]any)
	secondContent, _ := second["content"].([]any)
	block, _ := secondContent[0].(map[string]any)
	if got, _ := block["text"].(string); got != "Hi again" {
		t.Fatalf("expected text block content to be scrubbed, got %q", got)
	}
}

func TestAnthropicResponseNoticeStreamInjectsBeforeStop(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	notice := observeModeInjectedUserNotice("agent_123", "http://127.0.0.1:25297")

	body := newToolUseStreamBody(io.NopCloser(strings.NewReader(stream)), newAnthropicResponseNoticeStreamProcessor(notice))
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, notice) || !strings.Contains(got, `"text":"`) || !strings.Contains(got, `hello`) {
		t.Fatalf("expected stream to contain observe notice, got:\n%s", got)
	}
	if strings.Contains(got, `"index":1`) {
		t.Fatalf("expected anthropic notice to prefix the first text block, got extra injected block:\n%s", got)
	}
}

func TestOpenAIResponsesNoticeStreamInjectsBeforeCompleted(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		``,
	}, "\n")
	notice := observeModeInjectedUserNotice("agent_123", "http://127.0.0.1:25297")

	body := newToolUseStreamBody(io.NopCloser(strings.NewReader(stream)), newOpenAIResponsesNoticeStreamProcessor(notice))
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, notice) || !strings.Contains(got, `"delta":"`) || !strings.Contains(got, `hello`) {
		t.Fatalf("expected stream to contain observe notice, got:\n%s", got)
	}
	if strings.Contains(got, `"output_index":1`) {
		t.Fatalf("expected openai responses notice to prefix the first text delta, got extra injected block:\n%s", got)
	}
}

func TestShouldEmitObserveNoticeSuppressesRecentEvent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/observe-notice.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "observe-notice-session", userID, agentID, true)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	srv := &Server{}
	if !srv.shouldEmitObserveNotice(ctx, st, runtimeSession) {
		t.Fatal("expected fresh session to emit observe notice")
	}
	emitRuntimeEvent(ctx, st, runtimeSession, nil, runtimeEventOptions{
		EventType:  observeNoticeEventType,
		ActionKind: "observe_mode",
		Outcome:    stringPtr("injected"),
	})
	if srv.shouldEmitObserveNotice(ctx, st, runtimeSession) {
		t.Fatal("expected recent observe notice event to suppress another emit")
	}
}

func TestShouldEmitObserveNoticeIgnoresUnrelatedEventFlood(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/observe-notice-flood.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "observe-notice-flood-session", userID, agentID, true)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	emitRuntimeEvent(ctx, st, runtimeSession, nil, runtimeEventOptions{
		EventType:  observeNoticeEventType,
		ActionKind: "observe_mode",
		Outcome:    stringPtr("injected"),
	})
	for i := 0; i < 200; i++ {
		emitRuntimeEvent(ctx, st, runtimeSession, nil, runtimeEventOptions{
			EventType:  "runtime.observe.would_review",
			ActionKind: "egress",
			Outcome:    stringPtr("observed"),
		})
	}

	srv := &Server{}
	if srv.shouldEmitObserveNotice(ctx, st, runtimeSession) {
		t.Fatal("expected recent observe notice to suppress another emit even after unrelated event flood")
	}
}
