package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeInChunks emulates the rewriter writing partial event data
// across multiple Write calls — the injector must line-buffer
// correctly when events arrive split mid-line.
func writeInChunks(t *testing.T, w io.Writer, body string, chunk int) {
	t.Helper()
	for i := 0; i < len(body); i += chunk {
		end := i + chunk
		if end > len(body) {
			end = len(body)
		}
		if _, err := w.Write([]byte(body[i:end])); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
}

func TestStreamingFirstTurnNoticeWriter_BlankTextIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeAnthropicMessages, "   ")
	if w != &buf {
		t.Fatalf("expected dest returned unchanged for blank text; got wrapper")
	}
}

func TestStreamingFirstTurnNoticeWriter_UnknownShapeIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeUnknown, "hi")
	if w != &buf {
		t.Fatalf("expected dest returned unchanged for unknown shape; got wrapper")
	}
}

func TestStreamingFirstTurnNoticeWriter_Anthropic(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}`,
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
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeAnthropicMessages, "[Clawvisor] notice")
	writeInChunks(t, w, upstream, 17) // 17 to force splits across line boundaries
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	got := buf.String()

	// Notice text appears once, before the upstream's "hello".
	noticePos := strings.Index(got, "[Clawvisor] notice")
	helloPos := strings.Index(got, "hello")
	if noticePos == -1 {
		t.Fatalf("notice text missing:\n%s", got)
	}
	if helloPos == -1 {
		t.Fatalf("upstream text missing:\n%s", got)
	}
	if noticePos > helloPos {
		t.Errorf("notice should precede upstream text:\n%s", got)
	}
	if strings.Count(got, "[Clawvisor] notice") != 1 {
		t.Errorf("notice should appear exactly once:\n%s", got)
	}

	// Original upstream text block (at index 0) should be shifted to
	// index 1. Walk the events and verify.
	events, err := parseSSEEvents([]byte(got))
	if err != nil {
		t.Fatalf("parseSSEEvents: %v", err)
	}

	var sawNoticeStart, sawNoticeDelta, sawNoticeStop bool
	var sawShiftedHello bool
	for _, ev := range events {
		var obj map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &obj); err != nil {
			continue
		}
		switch ev.Event {
		case "content_block_start":
			cb, _ := obj["content_block"].(map[string]any)
			idx := numAsInt(obj["index"])
			if cb != nil && cb["type"] == "text" {
				if idx == 0 && !sawNoticeStart {
					sawNoticeStart = true
				}
			}
		case "content_block_delta":
			delta, _ := obj["delta"].(map[string]any)
			idx := numAsInt(obj["index"])
			if delta != nil && delta["text"] == "[Clawvisor] notice" && idx == 0 {
				sawNoticeDelta = true
			}
			if delta != nil && delta["text"] == "hello" {
				if idx != 1 {
					t.Errorf("upstream hello delta should be at index 1; got %d", idx)
				}
				sawShiftedHello = true
			}
		case "content_block_stop":
			idx := numAsInt(obj["index"])
			if idx == 0 && !sawNoticeStop {
				sawNoticeStop = true
			}
		}
	}
	if !sawNoticeStart || !sawNoticeDelta || !sawNoticeStop {
		t.Errorf("notice events missing: start=%v delta=%v stop=%v",
			sawNoticeStart, sawNoticeDelta, sawNoticeStop)
	}
	if !sawShiftedHello {
		t.Errorf("upstream content_block_delta with text=hello not found in output")
	}
}

func TestStreamingFirstTurnNoticeWriter_OpenAIChat_MergesIntoFirstChunk(t *testing.T) {
	// First chunk carries role:"assistant" — the merge path must
	// rewrite its delta.content to carry the notice + "hi" instead of
	// emitting a separate role-bearing prefix chunk (which would
	// register as a second assistant turn in strict accumulators).
	upstream := `data: {"id":"chatcmpl_x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl_x","choices":[{"index":0,"delta":{"content":" there"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeOpenAIChat, "[Clawvisor] notice")
	writeInChunks(t, w, upstream, 13)
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	got := buf.String()

	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("[DONE] sentinel missing:\n%s", got)
	}
	if strings.Count(got, "[Clawvisor] notice") != 1 {
		t.Errorf("notice should appear exactly once:\n%s", got)
	}
	// The synthetic-prefix marker must NOT appear — that fallback only
	// fires when no upstream chunk was mergeable.
	if strings.Contains(got, "chatcmpl_clawvisor_notice") {
		t.Errorf("synthetic prefix chunk should not be emitted when first chunk is mergeable:\n%s", got)
	}
	// Walk parsed chat chunks and check the first one carries
	// role:"assistant" PLUS content that begins with the notice text.
	events, err := parseSSEEvents([]byte(got))
	if err != nil {
		t.Fatalf("parseSSEEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("no events parsed:\n%s", got)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(events[0].Data), &first); err != nil {
		t.Fatalf("first chunk not JSON: %v\n%s", err, events[0].Data)
	}
	choices, _ := first["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("first chunk missing choices: %v", first)
	}
	choice0, _ := choices[0].(map[string]any)
	delta, _ := choice0["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Errorf("first chunk should preserve role:\"assistant\"; got delta=%v", delta)
	}
	content, _ := delta["content"].(string)
	if !strings.HasPrefix(content, "[Clawvisor] notice") {
		t.Errorf("first chunk content should be prefixed with notice; got %q", content)
	}
	if !strings.Contains(content, "hi") {
		t.Errorf("first chunk content should still include original \"hi\"; got %q", content)
	}
	// Count role transitions across the stream — there must be
	// exactly one. A second role:"assistant" in any subsequent chunk
	// is the bug the merge path exists to prevent.
	roleCount := 0
	for _, ev := range events {
		var obj map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &obj); err != nil {
			continue
		}
		ch, _ := obj["choices"].([]any)
		for _, c := range ch {
			cm, _ := c.(map[string]any)
			d, _ := cm["delta"].(map[string]any)
			if _, ok := d["role"]; ok {
				roleCount++
			}
		}
	}
	if roleCount != 1 {
		t.Errorf("expected exactly one role transition in stream; got %d\n%s", roleCount, got)
	}
}

func TestStreamingFirstTurnNoticeWriter_OpenAIChat_SyntheticFallbackOnDoneOnly(t *testing.T) {
	// Stream carries only [DONE] — no mergeable chunk. The synthetic
	// fallback should fire so the notice still surfaces.
	upstream := `data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeOpenAIChat, "[Clawvisor] notice")
	writeInChunks(t, w, upstream, 5)
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	got := buf.String()
	if !strings.Contains(got, "chatcmpl_clawvisor_notice") {
		t.Errorf("expected synthetic notice chunk in [DONE]-only stream:\n%s", got)
	}
	if !strings.Contains(got, "[Clawvisor] notice") {
		t.Errorf("notice text missing:\n%s", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Errorf("[DONE] sentinel must still be forwarded:\n%s", got)
	}
}

func TestStreamingFirstTurnNoticeWriter_OpenAIResponses(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_x"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_real","role":"assistant","status":"in_progress"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_real","output_index":0,"content_index":0,"delta":"hello"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_real","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_x","output":[{"type":"message","id":"msg_real","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"output_text":"hello"}}`,
		``,
	}, "\n")

	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeOpenAIResponses, "[Clawvisor] notice")
	writeInChunks(t, w, upstream, 19)
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	got := buf.String()
	noticePos := strings.Index(got, "[Clawvisor] notice")
	helloPos := strings.Index(got, `"hello"`)
	if noticePos == -1 {
		t.Fatalf("notice text missing:\n%s", got)
	}
	if helloPos == -1 {
		t.Fatalf("upstream content missing:\n%s", got)
	}
	if noticePos > helloPos {
		t.Errorf("notice should precede upstream content:\n%s", got)
	}

	events, err := parseSSEEvents([]byte(got))
	if err != nil {
		t.Fatalf("parseSSEEvents: %v", err)
	}

	// Notice envelope is six events at output_index 0.
	var noticeEnvelope []string
	var shiftedHello bool
	var completedHasNotice bool
	for _, ev := range events {
		var obj map[string]any
		_ = json.Unmarshal([]byte(ev.Data), &obj)
		switch ev.Event {
		case "response.output_item.added", "response.content_part.added",
			"response.output_text.delta", "response.output_text.done",
			"response.content_part.done", "response.output_item.done":
			idx := numAsInt(obj["output_index"])
			itemID := ""
			if v, ok := obj["item_id"].(string); ok {
				itemID = v
			}
			if item, ok := obj["item"].(map[string]any); ok {
				if id, ok := item["id"].(string); ok {
					itemID = id
				}
			}
			switch {
			case itemID == "msg_clawvisor_notice":
				if idx != 0 {
					t.Errorf("notice event %q at unexpected index %d", ev.Event, idx)
				}
				noticeEnvelope = append(noticeEnvelope, ev.Event)
			case itemID == "msg_real":
				// Original upstream events should be shifted to index 1.
				if idx != 1 {
					t.Errorf("upstream event %q for msg_real should be at index 1; got %d", ev.Event, idx)
				}
				if delta, ok := obj["delta"].(string); ok && delta == "hello" {
					shiftedHello = true
				}
			}
		case "response.completed":
			resp, _ := obj["response"].(map[string]any)
			if resp == nil {
				continue
			}
			output, _ := resp["output"].([]any)
			if len(output) == 0 {
				continue
			}
			first, _ := output[0].(map[string]any)
			if first["id"] == "msg_clawvisor_notice" {
				completedHasNotice = true
			}
		}
	}
	if len(noticeEnvelope) < 6 {
		t.Errorf("expected six notice envelope events, got %d (%v)", len(noticeEnvelope), noticeEnvelope)
	}
	if !shiftedHello {
		t.Errorf("upstream output_text.delta with text=hello not found shifted to index 1")
	}
	if !completedHasNotice {
		t.Errorf("response.completed should have notice prepended to response.output[]\nGOT:\n%s", got)
	}
}

func TestStreamingFirstTurnNoticeWriter_PassThroughComments(t *testing.T) {
	// SSE allows `:` comments (vendor keepalives). The injector must
	// pass them through verbatim, not eat them as data lines.
	upstream := ": keepalive\n" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4"}}` + "\n\n" +
		": another keepalive\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"

	var buf bytes.Buffer
	w := NewStreamingFirstTurnNoticeWriter(&buf, StreamShapeAnthropicMessages, "[Clawvisor] notice")
	writeInChunks(t, w, upstream, 7)
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	got := buf.String()
	if !strings.Contains(got, ": keepalive") {
		t.Errorf("first keepalive comment lost:\n%s", got)
	}
	if !strings.Contains(got, ": another keepalive") {
		t.Errorf("second keepalive comment lost:\n%s", got)
	}
}

func TestDetectStreamShape(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		provider Provider
		want     StreamShape
	}{
		{"anthropic_messages", "/v1/messages", ProviderAnthropic, StreamShapeAnthropicMessages},
		{"openai_chat", "/v1/chat/completions", ProviderOpenAI, StreamShapeOpenAIChat},
		{"openai_responses", "/v1/responses", ProviderOpenAI, StreamShapeOpenAIResponses},
		{"unknown_provider", "/v1/anything", Provider(""), StreamShapeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, nil)
			got := DetectStreamShape(req, tc.provider)
			if got != tc.want {
				t.Errorf("DetectStreamShape(%s, %s) = %d; want %d", tc.path, tc.provider, got, tc.want)
			}
		})
	}
}

func numAsInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return -1
}
