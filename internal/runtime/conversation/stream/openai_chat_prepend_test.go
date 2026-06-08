package stream_test

import (
	"bytes"
	"encoding/json"
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
	// Synthetic chunk carries content:<notice> and intentionally omits
	// role so clients do not see a duplicate assistant-role marker.
	if !strings.Contains(got, `chatcmpl_clawvisor_notice`) {
		t.Errorf("expected synthetic notice chunk ID present:\n%s", got)
	}
	firstData := strings.TrimPrefix(strings.SplitN(got, "\n\n", 2)[0], "data: ")
	var firstChunk struct {
		Choices []struct {
			Delta map[string]string `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(firstData), &firstChunk); err != nil {
		t.Fatalf("parse synthetic chunk: %v\n%s", err, firstData)
	}
	if len(firstChunk.Choices) != 1 {
		t.Fatalf("synthetic chunk choices = %d, want 1", len(firstChunk.Choices))
	}
	if _, ok := firstChunk.Choices[0].Delta["role"]; ok {
		t.Fatalf("synthetic chunk should omit role:\n%s", firstData)
	}
	if firstChunk.Choices[0].Delta["content"] != notice {
		t.Fatalf("synthetic chunk content = %q, want %q", firstChunk.Choices[0].Delta["content"], notice)
	}
	// Upstream "hello" + " world" + [DONE] all survive.
	for _, want := range []string{`"role":"assistant"`, `"content":"hello"`, `"content":" world"`, `data: [DONE]`} {
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

func TestPrependOpenAIChatAssistantNotice_OnlyDoneStillEmitsNotice(t *testing.T) {
	upstream := "data: [DONE]\n\n"
	const notice = "[Clawvisor] only done"

	var buf bytes.Buffer
	if err := stream.PrependOpenAIChatAssistantNotice(&buf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependOpenAIChatAssistantNotice: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, notice) {
		t.Fatalf("notice missing when upstream had no mergeable chunk:\n%s", got)
	}
	if !strings.HasSuffix(got, upstream) {
		t.Fatalf("upstream DONE sentinel should survive after notice\n--- got ---\n%s", got)
	}
	if strings.Index(got, notice) >= strings.Index(got, "data: [DONE]") {
		t.Fatalf("notice should precede DONE sentinel:\n%s", got)
	}
}

func TestPrependOpenAIChatAssistantNotice_SynthesizesDoneWhenMissing(t *testing.T) {
	upstream := `data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`
	const notice = "[Clawvisor] truncated upstream"

	var buf bytes.Buffer
	if err := stream.PrependOpenAIChatAssistantNotice(&buf, strings.NewReader(upstream), notice); err != nil {
		t.Fatalf("PrependOpenAIChatAssistantNotice: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, notice) || !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("notice or upstream content missing:\n%s", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("missing synthesized DONE sentinel:\n%s", got)
	}
}
