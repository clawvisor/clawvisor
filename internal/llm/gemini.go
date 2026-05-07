// Package llm — Gemini-on-Vertex provider path.
//
// Differs from the Anthropic-on-Vertex path (completeVertex) in three ways:
//   - Different request shape (contents/systemInstruction/generationConfig).
//   - Different response shape (candidates[].content.parts[].text).
//   - Different caching mechanism: explicit cachedContents resources created
//     out-of-band; the client references them by full resource path. The
//     resource name is discovered via geminiCacheNameFn (set by the cache
//     manager); when empty, the client falls through to inlining the
//     systemInstruction.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrGeminiCacheNotFound is wrapped by doGeminiRequest when Vertex returns
// status 404 on a request that referenced a cachedContent resource. The
// caller (completeGemini) treats it as a signal to retry once with the
// system prompt inlined, so a server-side cache eviction (TTL elapsed,
// resource manually deleted, refresh failure) doesn't turn into a
// user-visible error. Cache manager refresh continues independently.
var ErrGeminiCacheNotFound = errors.New("gemini: cached content not found")

// completeGemini sends generateContent to Vertex Gemini and returns text + usage.
//
// The endpoint URL is expected to already point at the model's
// :generateContent path (NewClient builds it from project/region/model).
//
// When the cache name function returns a non-empty string, the request body
// includes "cachedContent" pointing at that resource and OMITS
// systemInstruction (the two are mutually exclusive on the API side). When
// the function returns "" — either because no cache manager is attached or
// because the manager is recovering from a failed refresh — we inline the
// systemInstruction taken from the first system message in `messages`.
func (c *Client) completeGemini(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	if c.tokenSource == nil {
		return "", nil, fmt.Errorf("llm: gemini provider requires application default credentials")
	}

	// Same system/convo split as Anthropic.
	var system string
	var convo []ChatMessage
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
			}
			continue
		}
		convo = append(convo, m)
	}

	cacheName := ""
	if c.geminiCacheNameFn != nil {
		cacheName = c.geminiCacheNameFn()
	}

	text, usage, err := c.doGeminiRequest(ctx, system, convo, cacheName)
	if err != nil && cacheName != "" && errors.Is(err, ErrGeminiCacheNotFound) {
		// The cached resource is gone server-side — TTL elapsed before our
		// next refresh tick succeeded, or the manager's refresh has been
		// failing. Retry once with the system prompt inlined; the next
		// successful refresh will repopulate the cache and subsequent
		// calls go back to the cached path.
		return c.doGeminiRequest(ctx, system, convo, "")
	}
	return text, usage, err
}

// doGeminiRequest performs a single generateContent call. When cacheName
// is non-empty the request body references it (and omits systemInstruction);
// when empty, systemInstruction is inlined. Returns ErrGeminiCacheNotFound
// (wrapped) when status 404 is received with cacheName set, so the caller
// can decide whether to retry uncached.
func (c *Client) doGeminiRequest(ctx context.Context, system string, convo []ChatMessage, cacheName string) (string, *Usage, error) {
	thinkingLevel := c.geminiThinkingLevel
	if thinkingLevel == "" {
		thinkingLevel = "MINIMAL"
	}

	body := map[string]any{
		"contents": buildGeminiContents(convo),
		"generationConfig": map[string]any{
			"temperature":     0,
			"maxOutputTokens": c.effectiveMaxTokens(),
			"thinkingConfig": map[string]any{
				"thinkingLevel": thinkingLevel,
			},
		},
	}
	if cacheName != "" {
		body["cachedContent"] = cacheName
	} else if system != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", nil, err
	}
	tok, err := c.tokenSource.Token()
	if err != nil {
		return "", nil, fmt.Errorf("llm: gemini auth: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Limit only error responses — they're small JSON and we don't
		// want to slurp a multi-MB body just to format an error message.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if resp.StatusCode == http.StatusNotFound && cacheName != "" {
			return "", nil, fmt.Errorf("%w: url=%s body_len=%d server=%q via=%q content_type=%q body=%s",
				ErrGeminiCacheNotFound, c.endpoint, len(respBody),
				resp.Header.Get("Server"), resp.Header.Get("Via"),
				resp.Header.Get("Content-Type"), respBody)
		}
		return "", nil, fmt.Errorf("%w url=%s body_len=%d server=%q via=%q content_type=%q",
			c.statusError(resp.StatusCode, respBody),
			c.endpoint, len(respBody),
			resp.Header.Get("Server"), resp.Header.Get("Via"),
			resp.Header.Get("Content-Type"))
	}
	// Successful responses can be arbitrarily large depending on
	// maxOutputTokens (extractor uses 8192) plus per-part thoughtSignature
	// blobs. Read in full to avoid mid-document truncation that would crash
	// the JSON decoder on otherwise valid responses.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("llm: read gemini response: %w", err)
	}
	return decodeGeminiResponse(respBody)
}

// buildGeminiContents converts ChatMessage convo turns into the Gemini
// `contents` array format. Gemini uses "model" instead of "assistant".
func buildGeminiContents(convo []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(convo))
	for _, m := range convo {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		out = append(out, map[string]any{
			"role":  role,
			"parts": []map[string]any{{"text": m.Content}},
		})
	}
	return out
}

// decodeGeminiResponse parses a generateContent response body and returns
// the text of the first candidate plus a Usage filled from usageMetadata.
// CachedContentTokenCount is mapped onto Usage.CacheReadInputTokens so the
// existing telemetry helpers (LogUsage) report it under the same field
// name as Anthropic's cache_read_input_tokens.
func decodeGeminiResponse(body []byte) (string, *Usage, error) {
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
			TotalTokenCount         int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", nil, fmt.Errorf("llm: decode gemini response: %w", err)
	}
	usage := &Usage{
		// Vertex reports promptTokenCount as the TOTAL prompt input
		// (cached + uncached). Subtract cached so InputTokens reflects
		// only the freshly-processed part — same semantics as Anthropic's
		// usage.input_tokens.
		InputTokens:          out.UsageMetadata.PromptTokenCount - out.UsageMetadata.CachedContentTokenCount,
		OutputTokens:         out.UsageMetadata.CandidatesTokenCount,
		CacheReadInputTokens: out.UsageMetadata.CachedContentTokenCount,
		// Gemini doesn't have a separate "cache creation" event the way
		// Anthropic does; cache creation happens out-of-band via the
		// cachedContents API. Leave CacheCreationInputTokens at zero.
	}
	if usage.InputTokens < 0 {
		usage.InputTokens = 0 // defensive
	}
	if len(out.Candidates) == 0 {
		return "", usage, fmt.Errorf("llm: no candidates in gemini response")
	}
	for _, p := range out.Candidates[0].Content.Parts {
		if p.Text != "" {
			return p.Text, usage, nil
		}
	}
	return "", usage, fmt.Errorf("llm: no text part in gemini response (finish=%s)", out.Candidates[0].FinishReason)
}
