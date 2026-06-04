package llmproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// sseFromEvents joins (event, data) pairs into the SSE wire shape
// Anthropic uses for streaming.
func sseFromEvents(events ...[2]string) string {
	var sb strings.Builder
	for _, e := range events {
		sb.WriteString("event: ")
		sb.WriteString(e[0])
		sb.WriteString("\ndata: ")
		sb.WriteString(e[1])
		sb.WriteString("\n\n")
	}
	return sb.String()
}

const cbsThinking = `{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":"","signature":""}}`
const cbsText = `{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`
const cbsToolUse = `{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`

func TestPeekDetectsConsecutiveThinking(t *testing.T) {
	t.Parallel()
	stream := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_x"}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`},
	)
	prefix, decision, err := peekForConsecutiveThinking(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if decision != peekDecisionRetry {
		t.Errorf("decision=%q want retry; prefix=%s", decision, prefix)
	}
}

func TestPeekCommitsOnTextAfterThinking(t *testing.T) {
	t.Parallel()
	stream := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
	)
	_, decision, err := peekForConsecutiveThinking(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if decision != peekDecisionCommit {
		t.Errorf("decision=%q want commit", decision)
	}
}

func TestPeekCommitsOnToolUseAfterThinking(t *testing.T) {
	t.Parallel()
	stream := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Bash"}}`},
	)
	_, decision, err := peekForConsecutiveThinking(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if decision != peekDecisionCommit {
		t.Errorf("decision=%q want commit", decision)
	}
}

func TestPeekCommitsOnMessageStopWithSingleBlock(t *testing.T) {
	t.Parallel()
	stream := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta"}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	_, decision, err := peekForConsecutiveThinking(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if decision != peekDecisionCommit {
		t.Errorf("decision=%q want commit", decision)
	}
}

func TestPeekDetectsRedactedThinkingAdjacency(t *testing.T) {
	t.Parallel()
	stream := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking"}}`},
	)
	_, decision, err := peekForConsecutiveThinking(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if decision != peekDecisionRetry {
		t.Errorf("decision=%q want retry (thinking → redacted_thinking is also consecutive)", decision)
	}
}

func TestPeekPrefixIsByteFaithful(t *testing.T) {
	t.Parallel()
	stream := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	)
	prefix, _, err := peekForConsecutiveThinking(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Every byte read from the source must be in the prefix verbatim.
	if !strings.HasPrefix(stream, string(prefix)) {
		t.Errorf("prefix is not a strict prefix of the source stream:\n  prefix: %q\n  stream: %q", prefix, stream)
	}
}

// fakeForward returns a sequence of canned responses, advancing one per call.
func fakeForward(responses ...string) func() (*http.Response, error) {
	i := 0
	return func() (*http.Response, error) {
		if i >= len(responses) {
			return nil, io.EOF
		}
		body := responses[i]
		i++
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func TestForwardRetriesOnMultiThinking(t *testing.T) {
	t.Parallel()
	multiThinking := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Bash"}}`},
		[2]string{"message_delta", `{"type":"message_delta"}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	clean := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"hi"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"message_delta", `{"type":"message_delta"}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	fwd := fakeForward(multiThinking, clean)
	resp, stats, err := ForwardWithMultiThinkingRetry(context.Background(), fwd, MultiThinkingRetryConfig{MaxRetries: 2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !stats.Detected {
		t.Errorf("stats.Detected = false, expected true")
	}
	if stats.Retries != 1 {
		t.Errorf("stats.Retries = %d, expected 1", stats.Retries)
	}
	if stats.Exhausted {
		t.Errorf("stats.Exhausted = true, expected false")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != clean {
		t.Errorf("body mismatch:\n got: %q\nwant: %q", got, clean)
	}
}

func TestForwardExhaustsRetries(t *testing.T) {
	t.Parallel()
	multiThinking := sseFromEvents(
		[2]string{"message_start", `{"type":"message_start"}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Bash"}}`},
		[2]string{"message_delta", `{"type":"message_delta"}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	// Three multi-thinking responses in a row, retry cap of 2.
	fwd := fakeForward(multiThinking, multiThinking, multiThinking)
	resp, stats, err := ForwardWithMultiThinkingRetry(context.Background(), fwd, MultiThinkingRetryConfig{MaxRetries: 2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !stats.Exhausted {
		t.Errorf("stats.Exhausted = false, expected true after retry cap")
	}
	// Retries is the count of additional upstream calls beyond the
	// first. With MaxRetries=2 and three failed responses we performed
	// 2 retries before giving up.
	if stats.Retries != 2 {
		t.Errorf("stats.Retries = %d, expected 2 (additional retries beyond the first)", stats.Retries)
	}
	if resp == nil {
		t.Fatalf("expected non-nil resp after exhausted retries")
	}
	// Critical: the returned body must contain the full upstream
	// stream (prefix + rest), not just the buffered prefix. Without
	// this the caller would relay a truncated SSE that ends mid-block.
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != multiThinking {
		t.Errorf("exhausted response truncated.\n got len=%d\nwant len=%d", len(got), len(multiThinking))
	}
}

func TestForwardPassesThroughNonStreaming(t *testing.T) {
	t.Parallel()
	called := 0
	fwd := func() (*http.Response, error) {
		called++
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_x","content":[{"type":"text","text":"hi"}]}`)),
		}, nil
	}
	resp, stats, err := ForwardWithMultiThinkingRetry(context.Background(), fwd, MultiThinkingRetryConfig{MaxRetries: 2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if called != 1 {
		t.Errorf("forward called %d times, expected 1 (no peek/retry on non-streaming)", called)
	}
	if stats.Detected || stats.Retries != 0 {
		t.Errorf("expected no detection or retry, got %+v", stats)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(got, []byte("text")) {
		t.Errorf("body lost on non-streaming passthrough")
	}
}

func TestForwardPassesThroughErrorStatus(t *testing.T) {
	t.Parallel()
	called := 0
	fwd := func() (*http.Response, error) {
		called++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad"}`)),
		}, nil
	}
	resp, stats, err := ForwardWithMultiThinkingRetry(context.Background(), fwd, MultiThinkingRetryConfig{MaxRetries: 2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if called != 1 {
		t.Errorf("forward called %d times, expected 1 (no peek/retry on non-200)", called)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status passthrough broken")
	}
	_ = stats
}

func TestRequestWantsThinking(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"adaptive thinking", `{"model":"opus","thinking":{"type":"adaptive"}}`, true},
		{"enabled thinking", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, true},
		{"thinking null", `{"thinking":null}`, false},
		{"thinking absent", `{"model":"opus"}`, false},
		{"thinking nested in metadata", `{"metadata":{"thinking":{"type":"adaptive"}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RequestWantsThinking([]byte(tc.body))
			if got != tc.want {
				t.Errorf("RequestWantsThinking(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
