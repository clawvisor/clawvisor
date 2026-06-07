package stream_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation/stream"
)

func TestStreamRoundTrip_CRLFAllCodecs(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		roundTrip func(*testing.T, string) string
	}{
		{
			name: "anthropic",
			input: strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_crlf"}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\r\n"),
			roundTrip: roundTripAnthropic,
		},
		{
			name: "openai chat",
			input: strings.Join([]string{
				`data: {"id":"chatcmpl_crlf","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\r\n"),
			roundTrip: roundTripOpenAIChat,
		},
		{
			name: "openai responses",
			input: strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_crlf"}}`,
				``,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_crlf"}}`,
				``,
			}, "\r\n"),
			roundTrip: roundTripOpenAIResponses,
		},
		{
			name: "google",
			input: strings.Join([]string{
				`data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}]}`,
				``,
			}, "\r\n"),
			roundTrip: roundTripGoogle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.roundTrip(t, tc.input); got != tc.input {
				t.Fatalf("CRLF round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", tc.input, got)
			}
		})
	}
}

func TestStreamRoundTrip_MultilineDataAllCodecs(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		roundTrip func(*testing.T, string) string
	}{
		{
			name: "anthropic",
			input: strings.Join([]string{
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,`,
				`data: "delta":{"type":"text_delta","text":"hello"}}`,
				``,
			}, "\n"),
			roundTrip: roundTripAnthropic,
		},
		{
			name: "openai chat",
			input: strings.Join([]string{
				`data: {"id":"chatcmpl_multi",`,
				`data: "object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
				``,
			}, "\n"),
			roundTrip: roundTripOpenAIChat,
		},
		{
			name: "openai responses",
			input: strings.Join([]string{
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","output_index":0,`,
				`data: "content_index":0,"delta":"hello"}`,
				``,
			}, "\n"),
			roundTrip: roundTripOpenAIResponses,
		},
		{
			name: "google",
			input: strings.Join([]string{
				`data: {"candidates":[{"content":{"parts":[`,
				`data: {"text":"hello"}],"role":"model"}}]}`,
				``,
			}, "\n"),
			roundTrip: roundTripGoogle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.roundTrip(t, tc.input); got != tc.input {
				t.Fatalf("multi-line data round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", tc.input, got)
			}
		})
	}
}

func TestStreamRoundTrip_CommentsUnknownLinesAroundEvents(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		roundTrip func(*testing.T, string) string
	}{
		{
			name: "anthropic",
			input: strings.Join([]string{
				`: before`,
				`retry: 1000`,
				`event: message_start`,
				`data: {"type":"message_start"}`,
				``,
				`: between`,
				`id: abc`,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
				`: after`,
				``,
			}, "\n"),
			roundTrip: roundTripAnthropic,
		},
		{
			name: "openai chat",
			input: strings.Join([]string{
				`: before`,
				`retry: 1000`,
				`data: {"id":"chatcmpl_lines","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
				``,
				`: between`,
				`id: abc`,
				`data: [DONE]`,
				``,
				`: after`,
				``,
			}, "\n"),
			roundTrip: roundTripOpenAIChat,
		},
		{
			name: "openai responses",
			input: strings.Join([]string{
				`: before`,
				`retry: 1000`,
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_lines"}}`,
				``,
				`: between`,
				`id: abc`,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_lines"}}`,
				``,
				`: after`,
				``,
			}, "\n"),
			roundTrip: roundTripOpenAIResponses,
		},
		{
			name: "google",
			input: strings.Join([]string{
				`: before`,
				`retry: 1000`,
				`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}]}`,
				``,
				`: between`,
				`id: abc`,
				`data: {"candidates":[{"content":{"parts":[{"text":"done"}],"role":"model"},"finishReason":"STOP"}]}`,
				``,
				`: after`,
				``,
			}, "\n"),
			roundTrip: roundTripGoogle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.roundTrip(t, tc.input); got != tc.input {
				t.Fatalf("comment/unknown round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", tc.input, got)
			}
		})
	}
}

func TestStreamRoundTrip_PartialFinalEventAllCodecs(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		roundTrip func(*testing.T, string) string
	}{
		{
			name:      "anthropic",
			input:     "event: message_stop\ndata: {\"type\":\"message_stop\"}",
			roundTrip: roundTripAnthropic,
		},
		{
			name:      "openai chat",
			input:     "data: {\"id\":\"chatcmpl_partial\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"}}]}",
			roundTrip: roundTripOpenAIChat,
		},
		{
			name:      "openai responses",
			input:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_partial\"}}",
			roundTrip: roundTripOpenAIResponses,
		},
		{
			name:      "google",
			input:     "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"done\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}",
			roundTrip: roundTripGoogle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.roundTrip(t, tc.input); got != tc.input {
				t.Fatalf("partial final event round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", tc.input, got)
			}
		})
	}
}

func TestStreamRoundTrip_MalformedJSONPreservesRawAndContinues(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		input := strings.Join([]string{
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n")
		d := stream.NewAnthropicDecoder(strings.NewReader(input))
		ev, err := d.Next()
		if err != nil {
			t.Fatalf("decode malformed event: %v", err)
		}
		if ev.Kind != stream.KindUnknown {
			t.Fatalf("malformed event Kind = %v, want KindUnknown", ev.Kind)
		}
		ev, err = d.Next()
		if err != nil {
			t.Fatalf("decode following event: %v", err)
		}
		if ev.Kind != stream.KindResponseEnd {
			t.Fatalf("following event Kind = %v, want KindResponseEnd", ev.Kind)
		}
		if got := roundTripAnthropic(t, input); got != input {
			t.Fatalf("malformed stream round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", input, got)
		}
	})

	t.Run("openai responses", func(t *testing.T) {
		input := strings.Join([]string{
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","output_index":`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_ok"}}`,
			``,
		}, "\n")
		d := stream.NewOpenAIResponsesDecoder(strings.NewReader(input))
		ev, err := d.Next()
		if err != nil {
			t.Fatalf("decode malformed event: %v", err)
		}
		if ev.Kind != stream.KindUnknown {
			t.Fatalf("malformed event Kind = %v, want KindUnknown", ev.Kind)
		}
		ev, err = d.Next()
		if err != nil {
			t.Fatalf("decode following event: %v", err)
		}
		if ev.Kind != stream.KindResponseEnd {
			t.Fatalf("following event Kind = %v, want KindResponseEnd", ev.Kind)
		}
		if got := roundTripOpenAIResponses(t, input); got != input {
			t.Fatalf("malformed stream round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", input, got)
		}
	})
}

func TestOpenAIChatRoundTrip_AdjacentCRLFChunksWithoutBlankLines(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl_adjacent","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`data: {"id":"chatcmpl_adjacent","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"}}]}`,
		`data: [DONE]`,
		``,
	}, "\r\n")
	if got := roundTripOpenAIChat(t, input); got != input {
		t.Fatalf("adjacent CRLF chunks round-trip not byte-identical\n--- want ---\n%q\n--- got ---\n%q", input, got)
	}
}

func TestStreamDecoders_AllowLargeDataLineNearScannerCap(t *testing.T) {
	largeText := strings.Repeat("x", 3<<20)

	anthropicPayload, err := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": largeText},
	})
	if err != nil {
		t.Fatalf("marshal anthropic payload: %v", err)
	}
	responsesPayload, err := json.Marshal(map[string]any{
		"type":          "response.output_text.delta",
		"output_index":  0,
		"content_index": 0,
		"delta":         largeText,
	})
	if err != nil {
		t.Fatalf("marshal responses payload: %v", err)
	}
	googlePayload, err := json.Marshal(map[string]any{
		"candidates": []map[string]any{{
			"content": map[string]any{"role": "model", "parts": []map[string]string{{"text": largeText}}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal google payload: %v", err)
	}

	cases := []struct {
		name string
		next func() error
	}{
		{
			name: "anthropic",
			next: func() error {
				d := stream.NewAnthropicDecoder(strings.NewReader("event: content_block_delta\ndata: " + string(anthropicPayload) + "\n\n"))
				_, err := d.Next()
				return err
			},
		},
		{
			name: "openai chat",
			next: func() error {
				d := stream.NewOpenAIChatDecoder(strings.NewReader(`data: {"id":"chatcmpl_large","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + largeText + `"}}]}` + "\n\n"))
				_, err := d.Next()
				return err
			},
		},
		{
			name: "openai responses",
			next: func() error {
				d := stream.NewOpenAIResponsesDecoder(strings.NewReader("event: response.output_text.delta\ndata: " + string(responsesPayload) + "\n\n"))
				_, err := d.Next()
				return err
			},
		},
		{
			name: "google",
			next: func() error {
				d := stream.NewGoogleDecoder(strings.NewReader("data: " + string(googlePayload) + "\n\n"))
				_, err := d.Next()
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.next(); err != nil {
				t.Fatalf("decode large line near scanner cap: %v", err)
			}
		})
	}
}

func TestSubstituteAnthropicResponse_ClosesSlowUpstreamWithoutReading(t *testing.T) {
	src := &blockingReadCloser{closed: make(chan struct{})}
	var dst bytes.Buffer
	done := make(chan error, 1)

	go func() {
		done <- stream.SubstituteAnthropicResponse(&dst, src, "blocked")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SubstituteAnthropicResponse: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("SubstituteAnthropicResponse blocked waiting for upstream bytes")
	}
	select {
	case <-src.closed:
	default:
		t.Fatal("SubstituteAnthropicResponse returned without closing upstream")
	}
}

type blockingReadCloser struct {
	closed chan struct{}
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	select {}
}

func (b *blockingReadCloser) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func roundTripGoogle(t *testing.T, sse string) string {
	t.Helper()
	d := stream.NewGoogleDecoder(strings.NewReader(sse))
	var buf bytes.Buffer
	enc := stream.NewGoogleEncoder(&buf)
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	return buf.String()
}
