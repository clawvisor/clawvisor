package stream_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// TestPrependOpenAIResponsesAssistantNotice_NoticeAtIndex0 verifies
// the notice envelope lands at output_index 0 and the upstream item
// shifts to output_index 1.
func TestPrependOpenAIResponsesAssistantNotice_NoticeAtIndex0(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_x","model":"gpt-5","output":[]}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress"}}`,
		``,
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
		``,
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hello"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_x"}}`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var buf bytes.Buffer
	if err := stream.PrependOpenAIResponsesAssistantNotice(&buf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependOpenAIResponsesAssistantNotice: %v", err)
	}
	got := buf.String()

	// Notice text appears exactly once (in the synthetic notice envelope
	// — the data:"text":"hello" line carries upstream content, not notice).
	if c := strings.Count(got, notice); c == 0 {
		t.Errorf("notice missing from output:\n%s", got)
	}

	// Notice item ID present.
	if !strings.Contains(got, `msg_clawvisor_notice`) {
		t.Errorf("notice item ID missing:\n%s", got)
	}

	// Upstream msg_1 shifted from output_index 0 to output_index 1.
	if !strings.Contains(got, `"output_index":1`) || !strings.Contains(got, `msg_1`) {
		t.Errorf("upstream item didn't shift to output_index 1:\n%s", got)
	}
	if !strings.Contains(got, `"output":[{"content":[{"text":"`+notice) {
		t.Errorf("completed response output does not include notice item:\n%s", got)
	}

	// Upstream "hello" content survives.
	if !strings.Contains(got, `"text":"hello"`) {
		t.Errorf("upstream hello text lost:\n%s", got)
	}

	// response.created passes through (must appear before the notice).
	createdIdx := strings.Index(got, "response.created")
	noticeIdx := strings.Index(got, "msg_clawvisor_notice")
	if createdIdx < 0 || createdIdx >= noticeIdx {
		t.Errorf("response.created didn't precede notice:\n%s", got)
	}
}

// TestPrependOpenAIResponsesAssistantNotice_BlankIsCopy pins the
// blank-text short-circuit.
func TestPrependOpenAIResponsesAssistantNotice_BlankIsCopy(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n")

	var buf bytes.Buffer
	if err := stream.PrependOpenAIResponsesAssistantNotice(&buf, strings.NewReader(upstream), ""); err != nil {
		t.Fatalf("blank prepend: %v", err)
	}
	if got := buf.String(); got != upstream {
		t.Fatalf("blank notice should copy verbatim\n--- want ---\n%s\n--- got ---\n%s", upstream, got)
	}
}
