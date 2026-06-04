package llmproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// MultiThinkingRetryStats reports what happened during a multi-thinking-aware
// upstream call. Zero values mean the feature was inactive (e.g. non-streaming
// response or thinking not requested).
type MultiThinkingRetryStats struct {
	Retries      int  // additional upstream calls made beyond the first
	Detected     bool // true if at least one multi-thinking response was discarded
	Exhausted    bool // true if we hit the retry cap and gave up (last response returned anyway)
	PeekedBytes  int  // bytes buffered before commit decision (≈ first thinking block + second block_start event)
}

// MultiThinkingRetryConfig tunes the wrapper. Default cap of 2 retries
// is based on observed multi-thinking emission rate of ~0% over 50
// attempts in production — three consecutive multi-thinking responses
// is overwhelmingly unlikely.
type MultiThinkingRetryConfig struct {
	MaxRetries int
}

// ForwardWithMultiThinkingRetry calls forwardFn and inspects the
// upstream response stream. If the response is an Anthropic SSE stream
// whose first assistant message contains *consecutive* thinking blocks
// (two `content_block_start` events with `type:thinking` or
// `type:redacted_thinking` at adjacent indices), the response is
// discarded and forwardFn is invoked again, up to MaxRetries times.
//
// Background: Anthropic occasionally emits assistant turns with
// consecutive thinking blocks under `interleaved-thinking-2025-05-14`
// + `effort-2025-11-24` betas. The same API rejects those turns when
// they appear in conversation history on the next request with
// "thinking blocks ... cannot be modified". A fresh upstream call
// almost always produces a single-thinking response. See the bug
// report in PR comments for full repro and Anthropic correspondence.
//
// Returns the final response. If retries are exhausted, returns the
// last (still-multi-thinking) response — the caller may want to fall
// back to dropping the second thinking block in the relay path.
//
// The returned response.Body holds the original upstream stream with
// the bytes buffered during peek prepended via io.MultiReader, so the
// caller sees the full stream from the first byte.
func ForwardWithMultiThinkingRetry(
	ctx context.Context,
	forwardFn func() (*http.Response, error),
	cfg MultiThinkingRetryConfig,
) (*http.Response, MultiThinkingRetryStats, error) {
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	stats := MultiThinkingRetryStats{}
	var lastResp *http.Response
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		resp, err := forwardFn()
		if err != nil {
			return nil, stats, err
		}
		// Bypass on non-200 / non-SSE responses — nothing for us to
		// detect, and we should not double-charge upstream on errors.
		if resp.StatusCode != http.StatusOK || !isAnthropicSSEResponse(resp) {
			return resp, stats, nil
		}
		prefix, decision, peekErr := peekForConsecutiveThinking(resp.Body)
		stats.PeekedBytes = len(prefix)
		if peekErr != nil {
			// Couldn't decide — return the bytes we read prepended,
			// don't burn a retry on what may be an unrelated parse
			// failure or short read.
			resp.Body = wrapBodyWithPrefix(prefix, resp.Body)
			return resp, stats, nil
		}
		if decision != peekDecisionRetry {
			resp.Body = wrapBodyWithPrefix(prefix, resp.Body)
			return resp, stats, nil
		}
		// Multi-thinking detected — discard this response and retry.
		stats.Detected = true
		if lastResp != nil {
			_ = lastResp.Body.Close()
		}
		// Hold this response for fallback in case we exhaust retries.
		// We close the body here (since we've already buffered the
		// prefix and won't replay the rest) and reconstruct on
		// fallback if needed.
		_ = resp.Body.Close()
		lastResp = &http.Response{
			Status:        resp.Status,
			StatusCode:    resp.StatusCode,
			Header:        resp.Header.Clone(),
			Body:          io.NopCloser(bytes.NewReader(prefix)),
			ContentLength: int64(len(prefix)),
			Request:       resp.Request,
			Proto:         resp.Proto,
			ProtoMajor:    resp.ProtoMajor,
			ProtoMinor:    resp.ProtoMinor,
		}
		stats.Retries = attempt + 1
		// Honor cancellation.
		if err := ctx.Err(); err != nil {
			return lastResp, stats, err
		}
	}
	// Out of retries — return the last response we saw. The caller is
	// expected to handle this as a degraded case (e.g. drop the
	// second thinking block in the relay path).
	stats.Exhausted = true
	return lastResp, stats, nil
}

// isAnthropicSSEResponse reports whether the response Content-Type is
// `text/event-stream` (Anthropic streaming). We only retry streaming
// responses; JSON-only Anthropic responses with multi-thinking are
// not in scope here (and would require a different peek path).
func isAnthropicSSEResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// wrapBodyWithPrefix returns a ReadCloser that yields prefix followed
// by the remaining bytes from body. Closing the result closes the
// original body.
func wrapBodyWithPrefix(prefix []byte, body io.ReadCloser) io.ReadCloser {
	return &prefixedBody{
		r: io.MultiReader(bytes.NewReader(prefix), body),
		c: body,
	}
}

type prefixedBody struct {
	r io.Reader
	c io.Closer
}

func (p *prefixedBody) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *prefixedBody) Close() error               { return p.c.Close() }

const (
	peekDecisionCommit = "commit"
	peekDecisionRetry  = "retry"
)

// peekForConsecutiveThinking reads SSE events from body until it can
// decide whether the response contains *consecutive* thinking blocks
// (the multi-thinking pattern that triggers Anthropic's "thinking
// blocks cannot be modified" 400 on replay).
//
// Decision points:
//   - We see a content_block_start whose type is thinking AND the
//     immediately preceding content_block_start was also thinking →
//     return ("retry").
//   - We see a non-thinking content_block_start, or message_delta, or
//     message_stop → return ("commit"). The response is safe.
//   - We hit EOF before a decision → return ("commit") since a
//     well-formed response would have hit message_stop by then.
//
// The returned prefix contains every byte read from body so the
// caller can prepend it to the rest of the stream and let downstream
// processing see the full response.
func peekForConsecutiveThinking(body io.Reader) ([]byte, string, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(body, &buf)
	scanner := bufio.NewScanner(tee)
	// Anthropic SSE data lines can be large (~5KB+ for thinking
	// signatures). Lift the scanner's max token size accordingly.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	curEvent := ""
	var blockTypes []string // type per content_block_start, in order

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "event:") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			switch curEvent {
			case "content_block_start":
				blockType, ok := extractContentBlockType(data)
				if !ok {
					continue
				}
				prev := ""
				if n := len(blockTypes); n > 0 {
					prev = blockTypes[n-1]
				}
				blockTypes = append(blockTypes, blockType)
				if isThinkingType(blockType) && isThinkingType(prev) {
					return buf.Bytes(), peekDecisionRetry, nil
				}
				if !isThinkingType(blockType) && len(blockTypes) >= 1 {
					return buf.Bytes(), peekDecisionCommit, nil
				}
			case "message_delta", "message_stop":
				return buf.Bytes(), peekDecisionCommit, nil
			}
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return buf.Bytes(), "", err
	}
	return buf.Bytes(), peekDecisionCommit, nil
}

func extractContentBlockType(data string) (string, bool) {
	var cbs struct {
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(data), &cbs); err != nil {
		return "", false
	}
	if cbs.ContentBlock.Type == "" {
		return "", false
	}
	return cbs.ContentBlock.Type, true
}

func isThinkingType(t string) bool {
	return t == "thinking" || t == "redacted_thinking"
}

// RequestWantsThinking reports whether the request body opts into
// Anthropic's extended thinking. Used by callers to gate the retry
// wrapper — requests without thinking enabled cannot produce
// multi-thinking responses, so we shouldn't pay the peek cost.
func RequestWantsThinking(body []byte) bool {
	start, end, ok := findThinkingFieldRange(body)
	if !ok {
		return false
	}
	value := body[start:end]
	// Trim leading whitespace.
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t' || value[0] == '\n' || value[0] == '\r') {
		value = value[1:]
	}
	// `"thinking": null` and `"thinking": false` count as off.
	if bytes.HasPrefix(value, []byte("null")) || bytes.HasPrefix(value, []byte("false")) {
		return false
	}
	return true
}

func findThinkingFieldRange(body []byte) (int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return 0, 0, false
		}
		if key != "thinking" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, false
			}
			continue
		}
		// Scan past `:` and whitespace to find the value start.
		p := int(dec.InputOffset())
		for p < len(body) && body[p] != ':' {
			p++
		}
		if p >= len(body) {
			return 0, 0, false
		}
		p++
		for p < len(body) && (body[p] == ' ' || body[p] == '\t' || body[p] == '\n' || body[p] == '\r') {
			p++
		}
		valueStart := p
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return 0, 0, false
		}
		return valueStart, int(dec.InputOffset()), true
	}
	return 0, 0, false
}
