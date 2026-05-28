package conversation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrependAnthropicAssistantText_JSON(t *testing.T) {
	body := []byte(`{
		"id": "msg_abc",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4",
		"content": [
			{"type": "text", "text": "thinking"},
			{"type": "tool_use", "id": "toolu_1", "name": "Write", "input": {"path": "/tmp/x"}}
		],
		"stop_reason": "tool_use"
	}`)
	out, err := PrependAnthropicAssistantText("application/json", body, "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("PrependAnthropicAssistantText: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	// Top-level fields preserved.
	for _, k := range []string{"id", "model", "stop_reason"} {
		if parsed[k] == nil {
			t.Errorf("field %q lost in prepend", k)
		}
	}
	content, ok := parsed["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("expected 3 content blocks (notice + original 2); got %d: %v", len(content), content)
	}
	notice := content[0].(map[string]any)
	if notice["type"] != "text" || notice["text"] != "[Clawvisor] approved" {
		t.Errorf("notice block malformed: %v", notice)
	}
	// Original first block still in position 1.
	orig := content[1].(map[string]any)
	if orig["type"] != "text" || orig["text"] != "thinking" {
		t.Errorf("original text block lost: %v", orig)
	}
	// tool_use still in position 2, unmodified.
	tu := content[2].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "toolu_1" {
		t.Errorf("tool_use lost or mutated: %v", tu)
	}
}

func TestPrependAnthropicAssistantText_JSON_EmptyTextIsNoOp(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
	out, err := PrependAnthropicAssistantText("application/json", body, "  ")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected no-op for blank text; got %s", out)
	}
}

func TestPrependAnthropicAssistantText_SSE(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_x","role":"assistant","model":"claude-sonnet-4","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_orig","name":"Write","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	out, err := PrependAnthropicAssistantText("text/event-stream", []byte(sse), "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("PrependAnthropicAssistantText: %v", err)
	}
	got := string(out)

	// Notice text appears once, before the tool_use.
	noticePos := strings.Index(got, "[Clawvisor] approved")
	toolPos := strings.Index(got, "toolu_orig")
	if noticePos == -1 {
		t.Fatalf("notice text missing from output:\n%s", got)
	}
	if toolPos == -1 {
		t.Fatalf("original tool_use missing from output:\n%s", got)
	}
	if noticePos >= toolPos {
		t.Errorf("notice should come before tool_use; notice at %d, tool at %d\n%s", noticePos, toolPos, got)
	}

	// message_start preserved (still references msg_x).
	if !strings.Contains(got, `"id":"msg_x"`) {
		t.Errorf("message_start id lost:\n%s", got)
	}
	// message_delta + message_stop preserved.
	if !strings.Contains(got, "message_delta") || !strings.Contains(got, "message_stop") {
		t.Errorf("message tail events lost:\n%s", got)
	}

	// Original tool_use block's index was reindexed from 0 to 1.
	// We verify by parsing the SSE and locating the tool_use's
	// content_block_start event.
	if !strings.Contains(got, `"index":1`) {
		t.Errorf("original block index not shifted to 1:\n%s", got)
	}
	// The new text block uses index 0.
	if !strings.Contains(got, `"index":0`) {
		t.Errorf("new notice block index 0 missing:\n%s", got)
	}
}

func TestPrependOpenAIChatAssistantText_JSON_NullContent(t *testing.T) {
	// tool_calls-only response: content is null.
	body := []byte(`{
		"id": "chatcmpl-1",
		"object": "chat.completion",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{"id": "call_x", "type": "function", "function": {"name": "Write", "arguments": "{}"}}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	out, err := PrependOpenAIChatAssistantText("application/json", body, "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("PrependOpenAIChatAssistantText: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	choices := parsed["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "[Clawvisor] approved" {
		t.Errorf("content should be the notice; got %v", msg["content"])
	}
	// tool_calls preserved.
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Errorf("tool_calls lost: %v", msg["tool_calls"])
	}
}

func TestPrependOpenAIChatAssistantText_JSON_StringContent(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "doing the work"},
			"finish_reason": "stop"
		}]
	}`)
	out, err := PrependOpenAIChatAssistantText("application/json", body, "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	msg := parsed["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	c, ok := msg["content"].(string)
	if !ok {
		t.Fatalf("content not string: %T", msg["content"])
	}
	if !strings.HasPrefix(c, "[Clawvisor] approved") {
		t.Errorf("notice not prepended: %q", c)
	}
	if !strings.Contains(c, "doing the work") {
		t.Errorf("original content lost: %q", c)
	}
}

func TestPrependOpenAIChatAssistantText_SSE(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-sse","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_y","type":"function","function":{"name":"Write","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-sse","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-sse","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := PrependOpenAIChatAssistantText("text/event-stream", []byte(sse), "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := string(out)

	// Notice content shows up.
	if !strings.Contains(got, `"content":"[Clawvisor] approved"`) {
		t.Errorf("notice content delta missing:\n%s", got)
	}
	// Notice appears in the SAME chunk as the original first delta's
	// role/tool_calls — no second role transition. We assert this by
	// counting role:"assistant" occurrences: should be exactly one
	// (carried on the first upstream chunk, which is also where we
	// merged the notice content).
	roleCount := strings.Count(got, `"role":"assistant"`)
	if roleCount != 1 {
		t.Errorf("expected exactly one role:\"assistant\" transition (merged into first delta); got %d\n%s", roleCount, got)
	}
	// Notice precedes the tool_call arguments delta (it's on the
	// first chunk; the arguments stream in on the second).
	noticePos := strings.Index(got, "[Clawvisor] approved")
	argsPos := strings.Index(got, `"arguments":"{}"`)
	if noticePos == -1 || argsPos == -1 || noticePos >= argsPos {
		t.Errorf("notice should be before the args delta; notice at %d, args at %d\n%s", noticePos, argsPos, got)
	}
	// Original tool_call info (id + name) preserved on the merged
	// first chunk.
	if !strings.Contains(got, `"id":"call_y"`) || !strings.Contains(got, `"name":"Write"`) {
		t.Errorf("first-delta tool_call lost on merge:\n%s", got)
	}
	// Stream id preserved.
	if !strings.Contains(got, `"id":"chatcmpl-sse"`) {
		t.Errorf("chunk id not preserved:\n%s", got)
	}
	// finish_reason chunk passes through unchanged.
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason chunk lost:\n%s", got)
	}
}

// TestPrependOpenAIChatAssistantText_SSE_MultimodalContentArray
// confirms that when the upstream's first delta carries a content-parts
// array (multimodal/vision deltas), the prepend prepends a text part
// rather than collapsing the array to a string. Without this the
// image/audio parts on that delta are silently lost from the
// rendered turn.
func TestPrependOpenAIChatAssistantText_SSE_MultimodalContentArray(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-mm","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://example/img.png"}}]},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	out, err := PrependOpenAIChatAssistantText("text/event-stream", []byte(sse), "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := string(out)
	// Notice text appears as a prepended text part.
	if !strings.Contains(got, `[Clawvisor] approved`) {
		t.Errorf("notice missing:\n%s", got)
	}
	// Original text part survived.
	if !strings.Contains(got, `"text":"hi"`) {
		t.Errorf("original text part lost:\n%s", got)
	}
	// Image part survived — this is the load-bearing assertion
	// (previously the array got collapsed to a string and the image
	// disappeared).
	if !strings.Contains(got, `"image_url"`) || !strings.Contains(got, `"https://example/img.png"`) {
		t.Errorf("multimodal image part lost on prepend:\n%s", got)
	}
	// Content is still an array (not collapsed to a string).
	if !strings.Contains(got, `"content":[`) {
		t.Errorf("content should still be an array; appears collapsed:\n%s", got)
	}
}

// TestPrependOpenAIChatAssistantText_SSE_EmptyStreamFallsBack covers
// the malformed/empty-upstream case where there's no chunk to merge
// into. We fall back to a leading synthetic chunk so the notice still
// surfaces; this is the prior behavior.
func TestPrependOpenAIChatAssistantText_SSE_EmptyStreamFallsBack(t *testing.T) {
	out, err := PrependOpenAIChatAssistantText("text/event-stream", []byte("data: [DONE]\n\n"), "[Clawvisor] fallback")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "[Clawvisor] fallback") {
		t.Errorf("fallback path lost the notice:\n%s", got)
	}
}

func TestPrependOpenAIResponsesAssistantText_JSON(t *testing.T) {
	body := []byte(`{
		"id": "resp_1",
		"object": "response",
		"output": [
			{"type": "function_call", "id": "fc_1", "call_id": "call_x", "name": "Write", "arguments": "{}", "status": "completed"}
		],
		"output_text": ""
	}`)
	out, err := PrependOpenAIResponsesAssistantText("application/json", body, "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	output := parsed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 output items (notice + original); got %d", len(output))
	}
	notice := output[0].(map[string]any)
	if notice["type"] != "message" || notice["role"] != "assistant" {
		t.Errorf("first item should be assistant message; got %v", notice)
	}
	noticeContent := notice["content"].([]any)
	noticeText := noticeContent[0].(map[string]any)
	if noticeText["type"] != "output_text" || noticeText["text"] != "[Clawvisor] approved" {
		t.Errorf("notice text malformed: %v", noticeText)
	}
	// Original function_call still in position 1.
	orig := output[1].(map[string]any)
	if orig["type"] != "function_call" || orig["call_id"] != "call_x" {
		t.Errorf("original function_call lost: %v", orig)
	}
}

func TestPrependOpenAIResponsesAssistantText_SSE(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_sse"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_orig","call_id":"call_z","name":"Write","arguments":""}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_orig","call_id":"call_z","name":"Write","arguments":"{}","status":"completed"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_sse","status":"completed","output":[{"type":"function_call","id":"fc_orig","call_id":"call_z","name":"Write","arguments":"{}"}],"output_text":""}}`,
		``,
	}, "\n")

	out, err := PrependOpenAIResponsesAssistantText("text/event-stream", []byte(sse), "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := string(out)

	// response.created passes through unchanged.
	if !strings.Contains(got, `"id":"resp_sse"`) {
		t.Errorf("response.created lost:\n%s", got)
	}
	// Notice item events appear before the original function_call events.
	noticePos := strings.Index(got, "msg_clawvisor_notice")
	origPos := strings.Index(got, "fc_orig")
	if noticePos == -1 {
		t.Fatalf("notice item not emitted:\n%s", got)
	}
	if origPos == -1 {
		t.Fatalf("original item lost:\n%s", got)
	}
	if noticePos >= origPos {
		t.Errorf("notice should be before original; notice at %d, orig at %d\n%s", noticePos, origPos, got)
	}
	// Original event's output_index was shifted from 0 to 1.
	// (The notice item occupies index 0.)
	if !strings.Contains(got, `"output_index":1`) {
		t.Errorf("original output_index not shifted to 1:\n%s", got)
	}
	// FULL envelope shape — Codex CLI's renderer keys off
	// content_part.added + output_text.done to actually open and
	// close a visible text block, and (item_id, content_index)
	// to address it. Without all six events plus the keys, the
	// notice text is silently dropped from the rendered output.
	for _, eventName := range []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
	} {
		if !strings.Contains(got, "event: "+eventName) {
			t.Errorf("missing required envelope event %q for Codex CLI rendering:\n%s", eventName, got)
		}
	}
	// item_id + content_index must appear together on text-bearing
	// events so the renderer can address the open part.
	if !strings.Contains(got, `"item_id":"msg_clawvisor_notice"`) {
		t.Errorf("text-bearing events missing item_id; renderer can't address the notice part:\n%s", got)
	}
	if !strings.Contains(got, `"content_index":0`) {
		t.Errorf("text-bearing events missing content_index:\n%s", got)
	}
	// response.completed must carry the notice item at the start of
	// response.output and shift the original item to index 1. Clients
	// that reconcile from this terminal event would otherwise see a
	// final response without the notice despite earlier events
	// announcing it.
	completedIdx := strings.Index(got, "event: response.completed")
	if completedIdx == -1 {
		t.Fatalf("response.completed missing from output:\n%s", got)
	}
	completedTail := got[completedIdx:]
	if !strings.Contains(completedTail, `"id":"msg_clawvisor_notice"`) {
		t.Errorf("response.completed.response.output missing notice item:\n%s", completedTail)
	}
	if !strings.Contains(completedTail, `"id":"fc_orig"`) {
		t.Errorf("response.completed.response.output dropped original item:\n%s", completedTail)
	}
	// Notice item appears in output BEFORE the original — string-search
	// inside the completed event tail.
	noticeInCompleted := strings.Index(completedTail, "msg_clawvisor_notice")
	origInCompleted := strings.Index(completedTail, "fc_orig")
	if noticeInCompleted == -1 || origInCompleted == -1 || noticeInCompleted >= origInCompleted {
		t.Errorf("notice not before original in response.completed.output:\n%s", completedTail)
	}
}

func TestPrependAnthropicAssistantText_SSE_NoMessageStartFallsThrough(t *testing.T) {
	// Malformed: no message_start, just a content_block. We refuse
	// to inject the notice rather than corrupt the event order.
	sse := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
	}, "\n")
	out, err := PrependAnthropicAssistantText("text/event-stream", []byte(sse), "[Clawvisor] approved")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(string(out), "[Clawvisor] approved") {
		t.Errorf("notice should not be inserted into malformed stream (no message_start)")
	}
}
