package pipeline_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// TestStreamingResponseMutator_PrependAnthropicNotice exercises the
// real wiring: construct the mutator with an upstream reader + client
// writer, queue a PrependAssistantText, commit, and assert the output
// matches what the stream codec produces directly.
func TestStreamingResponseMutator_PrependAnthropicNotice(t *testing.T) {
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
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var dst bytes.Buffer
	m, err := pipeline.NewStreamingResponseMutator(&dst, strings.NewReader(upstream), conversation.StreamShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("NewStreamingResponseMutator: %v", err)
	}
	if err := m.PrependAssistantText(notice); err != nil {
		t.Fatalf("PrependAssistantText: %v", err)
	}
	if committer, ok := m.(interface{ Commit() error }); ok {
		if err := committer.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	} else {
		t.Fatalf("streaming mutator does not expose Commit()")
	}

	got := dst.String()
	if !strings.Contains(got, notice) {
		t.Errorf("expected notice in output, got:\n%s", got)
	}
	// Notice must precede the upstream "hello".
	if strings.Index(got, notice) >= strings.Index(got, "hello") {
		t.Errorf("notice did not precede hello:\n%s", got)
	}
	// Upstream block must shift to index 1.
	if !strings.Contains(got, `"index":1`) {
		t.Errorf("upstream block didn't shift to index 1:\n%s", got)
	}
}

// TestStreamingResponseMutator_NoMutationCopiesVerbatim verifies the
// "no policy queued any mutation" path: commit must copy upstream bytes
// to dst byte-identically. Critical for byte-fidelity of thinking
// blocks on responses no policy touches.
func TestStreamingResponseMutator_NoMutationCopiesVerbatim(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start"}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var dst bytes.Buffer
	m, err := pipeline.NewStreamingResponseMutator(&dst, strings.NewReader(upstream), conversation.StreamShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("NewStreamingResponseMutator: %v", err)
	}
	if committer, ok := m.(interface{ Commit() error }); ok {
		if err := committer.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	if got := dst.String(); got != upstream {
		t.Fatalf("expected byte-identical pass-through\n--- want ---\n%s\n--- got ---\n%s", upstream, got)
	}
}

// TestStreamingResponseMutator_RejectsCommitTwice pins the lifecycle
// invariant.
func TestStreamingResponseMutator_RejectsCommitTwice(t *testing.T) {
	src := strings.NewReader("event: foo\ndata: {}\n\n")
	var dst bytes.Buffer
	m, err := pipeline.NewStreamingResponseMutator(&dst, src, conversation.StreamShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("NewStreamingResponseMutator: %v", err)
	}
	committer := m.(interface{ Commit() error })
	if err := committer.Commit(); err != nil {
		t.Fatalf("first commit failed: %v", err)
	}
	if err := committer.Commit(); err == nil {
		t.Fatalf("expected second commit to error")
	}
}

// TestStreamingResponseMutator_RejectsUnsupportedShape pins the wire-
// up gate: only Anthropic Messages is supported today. OpenAI shapes
// arrive once their shape-specific prepend ports land.
func TestStreamingResponseMutator_RejectsUnsupportedShape(t *testing.T) {
	for _, shape := range []conversation.StreamShape{
		conversation.StreamShapeUnknown,
	} {
		_, err := pipeline.NewStreamingResponseMutator(&bytes.Buffer{}, strings.NewReader(""), shape)
		if err == nil {
			t.Errorf("shape %v: expected error, got nil", shape)
		}
	}
}

// TestStreamingResponseMutator_PrependOpenAIChatNotice exercises the
// new OpenAI Chat prepend wiring: a chat completions stream gets a
// synthetic leading chunk carrying the notice; upstream chunks pass
// through verbatim.
func TestStreamingResponseMutator_PrependOpenAIChatNotice(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	const notice = "[Clawvisor] notice"

	var dst bytes.Buffer
	m, err := pipeline.NewStreamingResponseMutator(&dst, strings.NewReader(upstream), conversation.StreamShapeOpenAIChat)
	if err != nil {
		t.Fatalf("NewStreamingResponseMutator: %v", err)
	}
	if err := m.PrependAssistantText(notice); err != nil {
		t.Fatalf("PrependAssistantText: %v", err)
	}
	if committer, ok := m.(interface{ Commit() error }); ok {
		if err := committer.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	got := dst.String()
	if !strings.Contains(got, notice) {
		t.Errorf("notice missing:\n%s", got)
	}
	if strings.Index(got, notice) >= strings.Index(got, `"hi"`) {
		t.Errorf("notice did not precede upstream content:\n%s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("upstream DONE sentinel lost:\n%s", got)
	}
}
