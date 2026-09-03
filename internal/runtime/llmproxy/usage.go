package llmproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pricing"
)

// ExtractUsageResult is what one upstream response told us about its
// token usage. Model echoes the upstream's `model` field, which may
// differ from the request-supplied model (Anthropic resolves aliases
// like `claude-opus-4-7-latest` to a concrete version on the way back).
// Found is false when no usage payload was present — caller should
// not record a cost row in that case.
type ExtractUsageResult struct {
	Found bool
	Model string
	Usage pricing.Usage
}

// ExtractUsage pulls the token-usage payload out of an upstream LLM
// response body. Handles both JSON and SSE shapes for Anthropic and
// OpenAI.
//
//   - Anthropic JSON:  body is a single `{"usage":{...}}` envelope.
//   - Anthropic SSE:   input tokens land in the `message_start` event,
//                      output tokens in the final `message_delta`.
//   - OpenAI JSON:     body has `usage:{prompt_tokens, completion_tokens,
//                      prompt_tokens_details.cached_tokens}`.
//   - OpenAI SSE:      usage appears in the final chunk only when the
//                      caller set `stream_options.include_usage=true`.
//                      Returns Found=false otherwise (no fabrication).
//
// requestModel falls back when the upstream body omits the model field
// (rare; happens on some error envelopes). Pass the model the caller
// asked for — it's still the right billing key.
func ExtractUsage(provider conversation.Provider, contentType string, body []byte, requestModel string) ExtractUsageResult {
	if len(body) == 0 {
		return ExtractUsageResult{}
	}
	// Detect SSE from the body itself, not just the Content-Type
	// header. Some upstream proxy paths drop or mangle the header by
	// the time it reaches us — we'd then fall through to the JSON
	// parser, fail to unmarshal an SSE stream, and silently return
	// Found=false. The body shape is unambiguous: SSE always opens
	// with an `event:` or `data:` line, while JSON opens with `{`.
	// Content-Type is consulted only as a tiebreaker for atypical
	// bodies.
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream") || bodyLooksLikeSSE(body)
	switch provider {
	case conversation.ProviderAnthropic:
		if isSSE {
			return extractAnthropicSSE(body, requestModel)
		}
		return extractAnthropicJSON(body, requestModel)
	case conversation.ProviderOpenAI:
		if isSSE {
			return extractOpenAISSE(body, requestModel)
		}
		return extractOpenAIJSON(body, requestModel)
	}
	return ExtractUsageResult{}
}

// bodyLooksLikeSSE returns true when the first non-whitespace byte
// of the body starts an `event:` or `data:` SSE field. Skips a UTF-8
// BOM if present (real SSE streams sometimes have one) before the
// prefix check.
func bodyLooksLikeSSE(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// Skip BOM + leading whitespace.
	i := 0
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		i = 3
	}
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\r' || body[i] == '\n') {
		i++
	}
	rest := body[i:]
	// Per the SSE spec a line starting with `:` is a comment, often
	// used by intermediaries as a keep-alive heartbeat (e.g. `: ping`)
	// before any real event arrives. A stream that opens with a
	// comment line is still SSE; treating it as JSON would silently
	// skip cost recording for the call.
	return bytes.HasPrefix(rest, []byte("event:")) ||
		bytes.HasPrefix(rest, []byte("data:")) ||
		bytes.HasPrefix(rest, []byte(":"))
}

type anthropicCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type anthropicUsage struct {
	InputTokens              int                    `json:"input_tokens"`
	OutputTokens             int                    `json:"output_tokens"`
	CacheCreationInputTokens int                    `json:"cache_creation_input_tokens"`
	CacheCreation            anthropicCacheCreation `json:"cache_creation"`
	CacheReadInputTokens     int                    `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) toPricing() pricing.Usage {
	// Prefer the per-TTL breakdown when the upstream surfaced it
	// (newer Anthropic versions emit cache_creation.{ephemeral_5m,
	// ephemeral_1h}_input_tokens). Fall back to the legacy
	// cache_creation_input_tokens scalar by treating it as 5-minute
	// cache writes — matches Anthropic's historical default TTL.
	write5m := u.CacheCreation.Ephemeral5mInputTokens
	write1h := u.CacheCreation.Ephemeral1hInputTokens
	if write5m == 0 && write1h == 0 {
		write5m = u.CacheCreationInputTokens
	}
	return pricing.Usage{
		InputTokens:        u.InputTokens,
		OutputTokens:       u.OutputTokens,
		CacheReadTokens:    u.CacheReadInputTokens,
		CacheWriteTokens:   write5m,
		CacheWrite1hTokens: write1h,
	}
}

type anthropicJSONEnvelope struct {
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"`
}

func extractAnthropicJSON(body []byte, requestModel string) ExtractUsageResult {
	var env anthropicJSONEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ExtractUsageResult{}
	}
	// A response that uses only the newer per-TTL fields (5m/1h
	// buckets nested under cache_creation) has the legacy scalar
	// at zero — include those in the zero-check or the row would
	// be silently dropped from cost recording.
	u := env.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 &&
		u.CacheCreation.Ephemeral5mInputTokens == 0 && u.CacheCreation.Ephemeral1hInputTokens == 0 {
		return ExtractUsageResult{}
	}
	model := env.Model
	if model == "" {
		model = requestModel
	}
	return ExtractUsageResult{Found: true, Model: model, Usage: env.Usage.toPricing()}
}

func extractAnthropicSSE(body []byte, requestModel string) ExtractUsageResult {
	// The stream's authoritative usage is assembled from two events:
	//   * message_start.message.usage carries input + cache tokens
	//     (and a placeholder output_tokens that the model overwrites).
	//   * the final message_delta.usage carries the real output_tokens.
	// We don't validate event ordering; just take the last non-zero
	// of each field as we walk the stream. That tolerates partial
	// streams (cancelled mid-flight) by reporting whatever did land.
	var merged anthropicUsage
	var model string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// Cap the per-line buffer at the body length itself. The body is
	// already bounded by h.MaxResponseBytes upstream, so this neither
	// uses more memory than the read already did nor lets a giant
	// single chunk silently truncate (which would have made usage
	// extraction return Found=false for the request).
	scanner.Buffer(make([]byte, 0, 64*1024), len(body)+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string         `json:"model"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message.Model != "" {
				model = ev.Message.Model
			}
			if ev.Message.Usage.InputTokens > 0 {
				merged.InputTokens = ev.Message.Usage.InputTokens
			}
			if ev.Message.Usage.CacheCreationInputTokens > 0 {
				merged.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
			}
			if ev.Message.Usage.CacheCreation.Ephemeral5mInputTokens > 0 {
				merged.CacheCreation.Ephemeral5mInputTokens = ev.Message.Usage.CacheCreation.Ephemeral5mInputTokens
			}
			if ev.Message.Usage.CacheCreation.Ephemeral1hInputTokens > 0 {
				merged.CacheCreation.Ephemeral1hInputTokens = ev.Message.Usage.CacheCreation.Ephemeral1hInputTokens
			}
			if ev.Message.Usage.CacheReadInputTokens > 0 {
				merged.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				merged.OutputTokens = ev.Usage.OutputTokens
			}
		}
	}
	// Found if anything we'd bill for landed. A cancelled stream
	// that delivered only message_start (prompt counted, no output
	// yet) is intentionally recorded — the user paid for the prompt
	// regardless of completion, and the cost row keeps OutputTokens
	// at zero rather than fabricating one.
	if merged.InputTokens == 0 && merged.OutputTokens == 0 &&
		merged.CacheReadInputTokens == 0 && merged.CacheCreationInputTokens == 0 &&
		merged.CacheCreation.Ephemeral5mInputTokens == 0 && merged.CacheCreation.Ephemeral1hInputTokens == 0 {
		return ExtractUsageResult{}
	}
	if model == "" {
		model = requestModel
	}
	return ExtractUsageResult{Found: true, Model: model, Usage: merged.toPricing()}
}

// openaiUsage accepts both shapes the OpenAI APIs return:
//   - Chat Completions / legacy: prompt_tokens, completion_tokens,
//     prompt_tokens_details.cached_tokens
//   - Responses API (/v1/responses): input_tokens, output_tokens,
//     input_tokens_details.cached_tokens
//
// Only one shape is populated per response; toPricing() picks
// whichever has non-zero token counts. Without this, Responses-API
// calls extract as zero and the cost row never lands.
type openaiUsage struct {
	// Chat Completions shape.
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// Responses-API shape.
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// hasTokens reports whether either shape populated anything billable.
// Used to distinguish "this response had no usage block" from "this
// response priced as a no-op."
func (u openaiUsage) hasTokens() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 ||
		u.InputTokens > 0 || u.OutputTokens > 0
}

func (u openaiUsage) toPricing() pricing.Usage {
	// Pick the shape that has data. Chat Completions returns
	// prompt_tokens / completion_tokens; Responses returns
	// input_tokens / output_tokens with the cached count nested
	// under input_tokens_details. Both shapes report the total
	// prompt as the headline number with the cached portion as a
	// sub-field, so the uncached-input split is identical.
	prompt := u.PromptTokens
	completion := u.CompletionTokens
	cached := u.PromptTokensDetails.CachedTokens
	if prompt == 0 && completion == 0 {
		prompt = u.InputTokens
		completion = u.OutputTokens
		cached = u.InputTokensDetails.CachedTokens
	}
	// The pricing table charges cache_read at a discount and the
	// remainder at the full input rate, so subtract cached out of
	// the input bucket to avoid double-billing the cached chunk.
	uncached := prompt - cached
	if uncached < 0 {
		uncached = 0
	}
	return pricing.Usage{
		InputTokens:     uncached,
		OutputTokens:    completion,
		CacheReadTokens: cached,
	}
}

type openaiJSONEnvelope struct {
	Model string      `json:"model"`
	Usage openaiUsage `json:"usage"`
}

func extractOpenAIJSON(body []byte, requestModel string) ExtractUsageResult {
	var env openaiJSONEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ExtractUsageResult{}
	}
	if !env.Usage.hasTokens() {
		return ExtractUsageResult{}
	}
	model := env.Model
	if model == "" {
		model = requestModel
	}
	return ExtractUsageResult{Found: true, Model: model, Usage: env.Usage.toPricing()}
}

func extractOpenAISSE(body []byte, requestModel string) ExtractUsageResult {
	// OpenAI SSE only emits usage when the caller set
	// `stream_options.include_usage=true` — when present it lands in
	// a final chunk with `usage:{...}` and an empty choices array.
	// Walk to the last chunk that carries a non-zero usage field.
	var last openaiUsage
	var model string
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// Cap the per-line buffer at the body length itself. The body is
	// already bounded by h.MaxResponseBytes upstream, so this neither
	// uses more memory than the read already did nor lets a giant
	// single chunk silently truncate (which would have made usage
	// extraction return Found=false for the request).
	scanner.Buffer(make([]byte, 0, 64*1024), len(body)+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		// Chat Completions SSE puts usage at the top level of each
		// chunk; Responses SSE nests it under `response`. Try both
		// shapes per event — whichever populated wins.
		var ev struct {
			Model    string       `json:"model"`
			Usage    *openaiUsage `json:"usage"`
			Response *struct {
				Model string       `json:"model"`
				Usage *openaiUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		if ev.Model != "" {
			model = ev.Model
		} else if ev.Response != nil && ev.Response.Model != "" {
			model = ev.Response.Model
		}
		usage := ev.Usage
		if usage == nil && ev.Response != nil {
			usage = ev.Response.Usage
		}
		if usage != nil && usage.hasTokens() {
			last = *usage
			found = true
		}
	}
	if !found {
		return ExtractUsageResult{}
	}
	if model == "" {
		model = requestModel
	}
	return ExtractUsageResult{Found: true, Model: model, Usage: last.toPricing()}
}
