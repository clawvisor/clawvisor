package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
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

// TestScrubHistoricalResponseNoticesFromAnthropicRequest_BackticksForm
// exercises the backticked `[Clawvisor] Observe mode: …` branch of
// observeNoticePrefixRE. Production emits this form, so a regression
// in the regex would leave the notice accumulating in echoed history
// on every turn — silently spamming the model. The legacy variants
// already have coverage in the test above; this one pins the current
// shape end-to-end through scrubHistoricalResponseNoticesFromRequest.
func TestScrubHistoricalResponseNoticesFromAnthropicRequest_BackticksForm(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	noticeWithSuffix := observeModeInjectedUserNotice("agent_123", "http://127.0.0.1:25297") + "\n\nHi there."
	standaloneNotice := observeModeInjectedUserNotice("agent_123", "")
	userQuoted := "Why does Clawvisor inject `[Clawvisor] Observe mode: …` into my history?"

	bodyMap := map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": standaloneNotice},
			}},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": noticeWithSuffix},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": userQuoted},
			}},
		},
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected backticked-form notice to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	firstContent, _ := first["content"].([]any)
	if len(firstContent) != 0 {
		t.Fatalf("expected standalone backticked notice block to be removed, got %d blocks", len(firstContent))
	}
	second, _ := messages[1].(map[string]any)
	secondContent, _ := second["content"].([]any)
	block, _ := secondContent[0].(map[string]any)
	if got, _ := block["text"].(string); got != "Hi there." {
		t.Fatalf("expected backticked notice prefix to be removed, got %q", got)
	}
	third, _ := messages[2].(map[string]any)
	thirdContent, _ := third["content"].([]any)
	userBlock, _ := thirdContent[0].(map[string]any)
	if got, _ := userBlock["text"].(string); got != userQuoted {
		t.Fatalf("expected user-authored quoted text mentioning the notice to be preserved, got %q", got)
	}
}

func TestScrubHistoricalResponseNoticeTextPreservesSimilarButNonExactPrefix(t *testing.T) {
	t.Parallel()

	text := "([Clawvisor system message]: Clawvisor is currently running in observe mode-ish for a documentation example.)\n\nKeep this."
	got, changed := scrubHistoricalResponseNoticeText(text)
	if changed {
		t.Fatalf("expected similar but non-exact prefix to be preserved, got %q", got)
	}
	if got != text {
		t.Fatalf("unexpected scrubbed text %q", got)
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

func TestShouldEmitObserveNoticeSuppressesConcurrentPendingEmit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/observe-notice-pending.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "observe-notice-pending-session", userID, agentID, true)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}

	srv := &Server{}
	if !srv.shouldEmitObserveNotice(ctx, st, runtimeSession) {
		t.Fatal("expected fresh session to emit observe notice")
	}
	if srv.shouldEmitObserveNotice(ctx, st, runtimeSession) {
		t.Fatal("expected second pending emit attempt to be suppressed until first notice is marked")
	}
}

// TestScrubHistoricalResponseNoticesFromAnthropicRequest_AutoApproveBanner
// pins the auto-approve banner shape emitted by
// llmproxy.AutoApproveUserNotice. Production accumulates one banner row
// per auto-approval; without the strip, every subsequent /v1/messages
// re-sends them all to the model.
func TestScrubHistoricalResponseNoticesFromAnthropicRequest_AutoApproveBanner(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	bannerWithPurpose := llmproxy.AutoApproveUserNotice("triage Gmail inbox, label and reply to threads")
	bannerWithSuffix := bannerWithPurpose + "\n\nOn it."
	bannerFallback := llmproxy.AutoApproveUserNotice("")
	userQuoted := "Can you explain what `[Clawvisor] Task auto-approved: ...` means?"

	bodyMap := map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": bannerFallback},
			}},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": bannerWithSuffix},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": userQuoted},
			}},
		},
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected auto-approve banner to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	firstContent, _ := first["content"].([]any)
	if len(firstContent) != 0 {
		t.Fatalf("expected standalone auto-approve banner block to be removed, got %d blocks", len(firstContent))
	}
	second, _ := messages[1].(map[string]any)
	secondContent, _ := second["content"].([]any)
	block, _ := secondContent[0].(map[string]any)
	if got, _ := block["text"].(string); got != "On it." {
		t.Fatalf("expected auto-approve banner prefix to be removed, got %q", got)
	}
	third, _ := messages[2].(map[string]any)
	thirdContent, _ := third["content"].([]any)
	userBlock, _ := thirdContent[0].(map[string]any)
	if got, _ := userBlock["text"].(string); got != userQuoted {
		t.Fatalf("expected user-authored quoted text mentioning the banner to be preserved, got %q", got)
	}
}

// TestScrubHistoricalResponseNoticesFromAnthropicRequest_RoutingNotice
// pins the first-turn routing notice shape emitted by
// llmproxy.RenderAgentRoutingNotice, including the optional trailing
// [clawvisor:conversation=...] marker. Routing fires once per fresh
// harness session but rides every subsequent turn's echoed history.
func TestScrubHistoricalResponseNoticesFromAnthropicRequest_RoutingNotice(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	routingWithName := llmproxy.RenderAgentRoutingNotice("claude-code-7", "cv-conv-abc123", "Clawvisor Local")
	routingWithSuffix := routingWithName + "\n\nWhat are you working on?"
	routingBrandOnly := llmproxy.RenderAgentRoutingNotice("", "", "")

	bodyMap := map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": routingBrandOnly},
			}},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": routingWithSuffix},
			}},
		},
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected routing notice to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	firstContent, _ := first["content"].([]any)
	if len(firstContent) != 0 {
		t.Fatalf("expected standalone routing notice block to be removed, got %d blocks", len(firstContent))
	}
	second, _ := messages[1].(map[string]any)
	secondContent, _ := second["content"].([]any)
	block, _ := secondContent[0].(map[string]any)
	if got, _ := block["text"].(string); got != "What are you working on?" {
		t.Fatalf("expected routing notice + conversation marker prefix to be removed, got %q", got)
	}
}

// TestScrubHistoricalResponseNoticesStripsConsecutiveMixedNotices
// covers the case where a single assistant text block leads with
// multiple Clawvisor notices stacked back-to-back — e.g., the routing
// notice on turn 1 followed immediately by an auto-approve banner from
// the same response. The loop in scrubHistoricalResponseNoticeText must
// drain ALL of them, not just the first.
func TestScrubHistoricalResponseNoticesStripsConsecutiveMixedNotices(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	routing := llmproxy.RenderAgentRoutingNotice("claude-code-7", "cv-conv-abc123", "Clawvisor")
	observe := observeModeInjectedUserNotice("agent_123", "http://127.0.0.1:25297")
	banner := llmproxy.AutoApproveUserNotice("fix the failing build")
	stacked := routing + "\n\n" + observe + "\n\n" + banner + "\n\nReal model output follows."

	bodyMap := map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": stacked},
			}},
		},
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected stacked notices to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].([]any)
	block, _ := content[0].(map[string]any)
	if got, _ := block["text"].(string); got != "Real model output follows." {
		t.Fatalf("expected all stacked notices to be drained, got %q", got)
	}
}

// TestScrubHistoricalResponseNoticesFromOpenAIRequest_AutoApproveBanner
// mirrors the Anthropic auto-approve test for the OpenAI Chat
// Completions provider shape (assistant.content as string).
func TestScrubHistoricalResponseNoticesFromOpenAIRequest_AutoApproveBanner(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", nil)
	banner := llmproxy.AutoApproveUserNotice("send the weekly status email")

	bodyMap := map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": banner + "\n\nDone."},
			{"role": "assistant", "content": llmproxy.AutoApproveUserNotice("")},
		},
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected openai chat auto-approve banners to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if got, _ := first["content"].(string); got != "Done." {
		t.Fatalf("expected auto-approve banner prefix to be removed from string content, got %q", got)
	}
	second, _ := messages[1].(map[string]any)
	if got, _ := second["content"].(string); got != "" {
		t.Fatalf("expected banner-only content to scrub to empty string, got %q", got)
	}
}

// TestScrubHistoricalResponseNoticesFromOpenAIResponsesRequest_RoutingNotice
// covers the OpenAI Responses (`input` array, `output_text` blocks)
// shape for the routing notice. The Responses scrubber walks `input`
// items with role=assistant and treats text/input_text/output_text
// block types as scrubbable.
func TestScrubHistoricalResponseNoticesFromOpenAIResponsesRequest_RoutingNotice(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	routing := llmproxy.RenderAgentRoutingNotice("claude-code-7", "cv-conv-abc123", "Clawvisor Staging")
	prefixed := routing + "\n\nKicking off."

	bodyMap := map[string]any{
		"input": []map[string]any{
			{"type": "message", "role": "assistant", "content": []map[string]any{
				{"type": "output_text", "text": prefixed},
			}},
		},
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	rewritten, changed := scrubHistoricalResponseNoticesFromRequest(req, body)
	if !changed {
		t.Fatal("expected openai responses routing notice to be scrubbed")
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	input, _ := payload["input"].([]any)
	first, _ := input[0].(map[string]any)
	content, _ := first["content"].([]any)
	block, _ := content[0].(map[string]any)
	if got, _ := block["text"].(string); got != "Kicking off." {
		t.Fatalf("expected routing notice prefix to be removed from output_text block, got %q", got)
	}
}

// TestScrubHistoricalResponseNoticeTextPreservesMidBlockBanner pins the
// anchor invariant: a banner-shaped substring in the MIDDLE of an
// assistant turn (e.g., the model quoting a banner back at the user)
// must not be stripped — only true leading notices belong to the proxy.
func TestScrubHistoricalResponseNoticeTextPreservesMidBlockBanner(t *testing.T) {
	t.Parallel()

	banner := llmproxy.AutoApproveUserNotice("explain the routing flow")
	mixed := "I see a notice like " + banner + " — what does it mean?"
	got, changed := scrubHistoricalResponseNoticeText(mixed)
	if changed {
		t.Fatalf("expected mid-block banner to be preserved, got change to %q", got)
	}
	if got != mixed {
		t.Fatalf("unexpected scrubbed text %q", got)
	}

	routing := llmproxy.RenderAgentRoutingNotice("claude-code-7", "cv-conv-abc123", "Clawvisor")
	mixedRouting := "Quoting back: " + routing
	gotRouting, changedRouting := scrubHistoricalResponseNoticeText(mixedRouting)
	if changedRouting {
		t.Fatalf("expected mid-block routing notice to be preserved, got change to %q", gotRouting)
	}
	if gotRouting != mixedRouting {
		t.Fatalf("unexpected scrubbed text %q", gotRouting)
	}
}

// TestScrubHistoricalResponseNoticesAcceptsUnicodePurpose covers the
// rune-truncation case in AutoApproveUserNotice: a purpose long enough
// to hit the 200-rune cap gets a `…` appended. The regex's purpose
// body `[^`+"`"+`]*` must accept the trailing `…` (a multi-byte UTF-8
// rune) without breaking. Without this row, an emoji or accented
// purpose that gets truncated could regress the regex match.
func TestScrubHistoricalResponseNoticesAcceptsUnicodePurpose(t *testing.T) {
	t.Parallel()

	// Build a purpose long enough to trigger rune-cap truncation in
	// AutoApproveUserNotice (>200 runes). Mixing ASCII + Unicode so the
	// truncation lands on a non-trivial byte position.
	long := strings.Repeat("作業 Task ", 60)
	banner := llmproxy.AutoApproveUserNotice(long)
	if !strings.HasSuffix(strings.TrimSuffix(banner, "`"), "…") {
		t.Fatalf("test precondition failed: expected truncated banner to end with …, got %q", banner)
	}

	got, changed := scrubHistoricalResponseNoticeText(banner + "\n\nReady.")
	if !changed {
		t.Fatalf("expected truncated unicode banner to be scrubbed, got %q", got)
	}
	if got != "Ready." {
		t.Fatalf("expected scrubbed remainder %q, got %q", "Ready.", got)
	}
}

