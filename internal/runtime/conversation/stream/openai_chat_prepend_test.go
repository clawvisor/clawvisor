package stream_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

// TestPrependOpenAIChatAssistantNotice_LeadingChunk verifies the
// notice surfaces as a synthetic leading chat.completion.chunk and
// every upstream chunk passes through verbatim afterward.
func TestPrependOpenAIChatAssistantNotice_LeadingChunk(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var buf bytes.Buffer
	if err := stream.PrependOpenAIChatAssistantNotice(&buf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependOpenAIChatAssistantNotice: %v", err)
	}

	got := buf.String()

	// Notice appears exactly once.
	if c := strings.Count(got, notice); c != 1 {
		t.Errorf("expected notice exactly once, got %d:\n%s", c, got)
	}
	// Notice precedes the upstream "hello".
	if strings.Index(got, notice) >= strings.Index(got, "hello") {
		t.Errorf("notice did not precede hello:\n%s", got)
	}
	// Synthetic chunk carries role:"assistant" + content:<notice>.
	if !strings.Contains(got, `chatcmpl_clawvisor_notice`) {
		t.Errorf("expected synthetic notice chunk ID present:\n%s", got)
	}
	// JSON key order is encoder-dependent, so assert on the individual
	// fields rather than the full substring.
	if !strings.Contains(got, `"role":"assistant"`) {
		t.Errorf("synthetic chunk missing role:assistant:\n%s", got)
	}
	if !strings.Contains(got, `"content":"[Clawvisor] notice"`) {
		t.Errorf("synthetic chunk missing notice content:\n%s", got)
	}
	// Upstream "hello" + " world" + [DONE] all survive.
	for _, want := range []string{`"content":"hello"`, `"content":" world"`, `data: [DONE]`} {
		if !strings.Contains(got, want) {
			t.Errorf("upstream content lost: %s\n%s", want, got)
		}
	}
}

// TestPrependOpenAIChatAssistantNotice_BlankIsCopy pins the blank-
// text short-circuit.
func TestPrependOpenAIChatAssistantNotice_BlankIsCopy(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var buf bytes.Buffer
	if err := stream.PrependOpenAIChatAssistantNotice(&buf, strings.NewReader(upstream), ""); err != nil {
		t.Fatalf("blank prepend: %v", err)
	}
	if got := buf.String(); got != upstream {
		t.Fatalf("blank notice should copy verbatim\n--- want ---\n%s\n--- got ---\n%s", upstream, got)
	}
}
