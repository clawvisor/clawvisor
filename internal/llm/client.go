// Package llm provides a thin HTTP client for LLM chat completions.
// Supports OpenAI-compatible endpoints (OpenAI, Groq, Ollama, …) and
// Anthropic's native Messages API.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ErrSpendCapExhausted is returned when a haiku proxy key (hkp_) has exceeded
// its spend cap. Callers should check for this with errors.Is to surface a
// user-facing prompt to provide their own API key.
var ErrSpendCapExhausted = errors.New("haiku proxy spend cap exhausted")

// ErrOverloaded is returned when the LLM provider signals it is overloaded
// (HTTP 529 or 503). Callers can check with errors.Is to apply back-off.
var ErrOverloaded = errors.New("llm provider overloaded")

const anthropicVersion       = "2023-06-01"
const vertexAnthropicVersion = "vertex-2023-10-16"

// defaultMaxTokens is the upper bound sent on every request when no per-client override is set.
// All use-cases (safety: ~50 tokens, conflicts: ~256, policy YAML: ~600) fit within 1024.
const defaultMaxTokens = 1024

// effectiveMaxTokens returns the max_tokens to use for this client.
func (c *Client) effectiveMaxTokens() int {
	if c.maxTokens > 0 {
		return c.maxTokens
	}
	return defaultMaxTokens
}

// buildSystemField returns the value for the Anthropic/Vertex "system" field.
// When cache is true, returns a single text block with an ephemeral cache_control
// breakpoint so the system prompt becomes a prompt-cache prefix. Otherwise
// returns the plain string form.
func buildSystemField(system string, cache bool) any {
	if !cache {
		return system
	}
	return []map[string]any{{
		"type":          "text",
		"text":          system,
		"cache_control": map[string]string{"type": "ephemeral"},
	}}
}

// ChatMessage is one turn in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"`
	// CacheControl marks this message as a prompt-cache breakpoint. Only honored
	// on Anthropic and Vertex providers, and only for system messages. Ignored
	// elsewhere.
	CacheControl bool `json:"-"`
}

// Client calls either an OpenAI-compatible, Anthropic, Vertex AI Anthropic,
// or Vertex AI Gemini chat endpoint.
type Client struct {
	provider          string
	endpoint          string
	apiKey            string
	model             string
	timeout           time.Duration
	http              *http.Client
	tokenSource       oauth2.TokenSource // for Vertex AI (ADC)
	maxTokens         int                // 0 → use default (maxTokens const)
	fallbackEndpoints []string           // Vertex AI: additional region endpoints to try on failure
	hedgeDelay        time.Duration      // 0 → no hedge; otherwise fire a second request after this delay

	// Gemini-specific settings.
	geminiThinkingLevel string       // "MINIMAL" | "LOW" | "MEDIUM" | "HIGH"; "" → MINIMAL
	geminiCacheNameFn   func() string // returns current Gemini cachedContents resource name; "" = uncached path
}

// WithMaxTokens returns a shallow copy of the client with a custom max_tokens limit.
func (c *Client) WithMaxTokens(n int) *Client {
	c2 := *c
	c2.maxTokens = n
	return &c2
}

// WithFallbackEndpoints returns a shallow copy of the client with fallback endpoints.
func (c *Client) WithFallbackEndpoints(endpoints []string) *Client {
	c2 := *c
	c2.fallbackEndpoints = endpoints
	return &c2
}

// WithTokenSource returns a shallow copy with the given token source (for testing).
func (c *Client) WithTokenSource(ts oauth2.TokenSource) *Client {
	c2 := *c
	c2.tokenSource = ts
	return &c2
}

// WithHedgeDelay returns a shallow copy with the given hedge delay. Pass 0
// to disable hedging. Used by tests; production wiring reads HedgeDelayMS
// from LLMProviderConfig in NewClient.
func (c *Client) WithHedgeDelay(d time.Duration) *Client {
	c2 := *c
	c2.hedgeDelay = d
	return &c2
}

// WithGeminiThinkingLevel returns a copy with a custom thinking-level
// override. Valid values: "MINIMAL", "LOW", "MEDIUM", "HIGH". Empty → MINIMAL.
func (c *Client) WithGeminiThinkingLevel(level string) *Client {
	c2 := *c
	c2.geminiThinkingLevel = level
	return &c2
}

// Endpoint returns the resolved endpoint URL the client posts to.
// Exposed primarily for tests that verify the endpoint construction logic.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// AttachGeminiCacheNameFn registers a function the client calls on every
// Gemini request to discover the current cachedContents resource name. The
// function returning "" causes the client to fall through to the uncached
// path (inlining systemInstruction). Mutates the receiver. Callers that
// build a fresh Client per request (e.g. verifier.Verify, extractor.ExtractLLM)
// must call this on each new instance.
func (c *Client) AttachGeminiCacheNameFn(fn func() string) {
	c.geminiCacheNameFn = fn
}

// NewClient builds a Client from a provider config.
func NewClient(cfg config.LLMProviderConfig) *Client {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if cfg.TimeoutSeconds == 0 {
		timeout = 10 * time.Second
	}
	provider := cfg.Provider
	if provider == "" {
		provider = "openai"
	}

	c := &Client{
		provider:   provider,
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		timeout:    timeout,
		http:       &http.Client{Timeout: timeout + 2*time.Second},
		hedgeDelay: time.Duration(cfg.HedgeDelayMS) * time.Millisecond,
	}

	if provider == "vertex" {
		ts, err := google.DefaultTokenSource(context.Background(),
			"https://www.googleapis.com/auth/cloud-platform",
		)
		if err == nil {
			c.tokenSource = ts
		}
		// Build the endpoint from env vars if not explicitly set.
		if c.endpoint == "" {
			region := os.Getenv("VERTEX_REGION")
			projectID := os.Getenv("VERTEX_PROJECT_ID")
			if region == "" {
				region = "us-east5"
			}
			c.endpoint = fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models",
				region, projectID, region)

			// Build fallback endpoints from VERTEX_FALLBACK_REGIONS (comma-separated).
			if fallback := os.Getenv("VERTEX_FALLBACK_REGIONS"); fallback != "" {
				for _, r := range strings.Split(fallback, ",") {
					r = strings.TrimSpace(r)
					if r != "" && r != region {
						c.fallbackEndpoints = append(c.fallbackEndpoints,
							fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models",
								r, projectID, r))
					}
				}
			}
		}
	}

	if provider == "gemini" {
		ts, err := google.DefaultTokenSource(context.Background(),
			"https://www.googleapis.com/auth/cloud-platform",
		)
		if err == nil {
			c.tokenSource = ts
		}
		c.geminiThinkingLevel = cfg.GeminiThinkingLevel
		// Build the :generateContent URL from project/region/model when
		// Endpoint isn't explicitly set. "global" uses the unprefixed
		// aiplatform.googleapis.com host; regional locations prefix it.
		if c.endpoint == "" {
			project := cfg.Project
			region := cfg.Region
			if region == "" {
				region = "global"
			}
			host := region + "-aiplatform.googleapis.com"
			if region == "global" {
				host = "aiplatform.googleapis.com"
			}
			c.endpoint = fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
				host, project, region, cfg.Model)
		}
	}

	return c
}

// statusError builds an error for a non-200 LLM response. If the key is a
// haiku proxy key and the status indicates the spend cap is exhausted (402 or
// 429), it wraps ErrSpendCapExhausted so callers can detect it with errors.Is.
func (c *Client) statusError(statusCode int, body []byte) error {
	base := fmt.Errorf("llm: %s %s status %d: %s", c.provider, c.model, statusCode, body)
	if strings.HasPrefix(c.apiKey, "hkp_") && (statusCode == http.StatusPaymentRequired || statusCode == http.StatusTooManyRequests) {
		return fmt.Errorf("%w: %w", ErrSpendCapExhausted, base)
	}
	if statusCode == 529 || statusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("%w: %w", ErrOverloaded, base)
	}
	return base
}

// Usage is provider-reported token accounting for a single completion.
// Cache fields are populated only by Anthropic and Vertex; OpenAI-compatible
// providers leave them at zero.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Complete sends a chat completion request and returns the assistant's reply.
func (c *Client) Complete(ctx context.Context, messages []ChatMessage) (string, error) {
	text, _, err := c.CompleteWithUsage(ctx, messages)
	return text, err
}

// CompleteWithUsage is like Complete but also returns provider usage info.
func (c *Client) CompleteWithUsage(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	if c.hedgeDelay <= 0 {
		return c.completeOnce(ctx, messages)
	}
	return c.completeWithHedge(ctx, messages)
}

// completeOnce dispatches a single request to the configured provider.
// Wrapped by completeWithHedge when hedgeDelay > 0.
func (c *Client) completeOnce(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	switch c.provider {
	case "anthropic":
		return c.completeAnthropic(ctx, messages)
	case "vertex":
		return c.completeVertex(ctx, messages)
	case "gemini":
		return c.completeGemini(ctx, messages)
	default:
		return c.completeOpenAI(ctx, messages) // "openai", "ollama", "groq" use OpenAI-compatible API
	}
}

// completeWithHedge fires a primary request and, if it hasn't returned
// after hedgeDelay, fires a second (hedge) request. Whichever succeeds
// first wins; the loser's context is cancelled.
//
// If the primary fails before the hedge fires, the error surfaces directly
// — no point hedging against a deterministic failure (auth error, bad
// model name, etc.). If both fail after racing, returns the most recent
// error.
func (c *Client) completeWithHedge(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	type result struct {
		text  string
		usage *Usage
		err   error
	}

	primaryCtx, cancelPrimary := context.WithCancel(ctx)
	defer cancelPrimary()
	primaryCh := make(chan result, 1)
	go func() {
		text, usage, err := c.completeOnce(primaryCtx, messages)
		primaryCh <- result{text, usage, err}
	}()

	timer := time.NewTimer(c.hedgeDelay)
	defer timer.Stop()

	select {
	case r := <-primaryCh:
		// Primary returned (success or failure) before hedge fired.
		return r.text, r.usage, r.err
	case <-ctx.Done():
		return "", nil, ctx.Err()
	case <-timer.C:
		// Primary slow — fire hedge below.
	}

	hedgeCtx, cancelHedge := context.WithCancel(ctx)
	defer cancelHedge()
	hedgeCh := make(chan result, 1)
	go func() {
		text, usage, err := c.completeOnce(hedgeCtx, messages)
		hedgeCh <- result{text, usage, err}
	}()

	// Race the two. First success wins; if one fails, wait for the other.
	var lastErr error
	for i := 0; i < 2; i++ {
		select {
		case r := <-primaryCh:
			if r.err == nil {
				cancelHedge()
				return r.text, r.usage, nil
			}
			lastErr = r.err
		case r := <-hedgeCh:
			if r.err == nil {
				cancelPrimary()
				return r.text, r.usage, nil
			}
			lastErr = r.err
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	return "", nil, lastErr
}

// ── OpenAI ────────────────────────────────────────────────────────────────────

func (c *Client) completeOpenAI(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	body, err := json.Marshal(map[string]any{
		"model":       c.model,
		"messages":    messages,
		"max_tokens":  c.effectiveMaxTokens(),
		"temperature": 0,
	})
	if err != nil {
		return "", nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", nil, c.statusError(resp.StatusCode, b)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, err
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("llm: no choices in response")
	}
	usage := &Usage{
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
	}
	return out.Choices[0].Message.Content, usage, nil
}

// ── Anthropic ─────────────────────────────────────────────────────────────────

func (c *Client) completeAnthropic(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	// Anthropic's Messages API separates the system prompt from the conversation.
	// Extract the first system message (if any); the rest must be user/assistant.
	var system string
	var systemCache bool
	var convo []ChatMessage
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
				systemCache = m.CacheControl
			}
			// Additional system messages are merged into the first.
			continue
		}
		convo = append(convo, m)
	}

	reqBody := map[string]any{
		"model":       c.model,
		"max_tokens":  c.effectiveMaxTokens(),
		"messages":    convo,
		"temperature": 0,
	}
	if system != "" {
		reqBody["system"] = buildSystemField(system, systemCache)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", nil, c.statusError(resp.StatusCode, b)
	}

	return decodeAnthropicResponse(resp.Body)
}

// ── Vertex AI ────────────────────────────────────────────────────────────────

func (c *Client) completeVertex(ctx context.Context, messages []ChatMessage) (string, *Usage, error) {
	if c.tokenSource == nil {
		return "", nil, fmt.Errorf("llm: vertex provider requires application default credentials")
	}

	// Build the list of endpoints to try: primary first, then fallbacks.
	endpoints := make([]string, 0, 1+len(c.fallbackEndpoints))
	endpoints = append(endpoints, c.endpoint)
	endpoints = append(endpoints, c.fallbackEndpoints...)

	// Same request body as Anthropic Messages API.
	var system string
	var systemCache bool
	var convo []ChatMessage
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
				systemCache = m.CacheControl
			}
			continue
		}
		convo = append(convo, m)
	}

	reqBody := map[string]any{
		"max_tokens":        c.effectiveMaxTokens(),
		"messages":          convo,
		"temperature":       0,
		"anthropic_version": vertexAnthropicVersion,
	}
	if system != "" {
		reqBody["system"] = buildSystemField(system, systemCache)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	var lastErr error
	for _, ep := range endpoints {
		// Try each region up to 2 times before moving to the next.
		for attempt := range 2 {
			text, usage, err := c.doVertexRequest(ctx, ep, body)
			if err == nil {
				return text, usage, nil
			}
			lastErr = err
			// Don't retry spend-cap errors at all.
			if errors.Is(err, ErrSpendCapExhausted) {
				return "", nil, err
			}
			if !isVertexRetriableErr(err) {
				return "", nil, err
			}
			// First attempt failed with retriable error — retry same region.
			// Second attempt failed — move to next region.
			_ = attempt
		}
	}
	return "", nil, lastErr
}

// isVertexRetriableErr checks whether the error is worth retrying on a
// different region: overloaded (529/503), retriable HTTP status, or network failure.
func isVertexRetriableErr(err error) bool {
	if err == nil {
		return false
	}
	// ErrOverloaded is wrapped by statusError for 529 and 503.
	if errors.Is(err, ErrOverloaded) {
		return true
	}
	// Network-level failures (timeout, connection refused, DNS).
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") {
		return true
	}
	// Remaining retriable HTTP statuses (429, 500, 502, 504) not covered by ErrOverloaded.
	for _, code := range []string{"status 429", "status 500", "status 502", "status 504"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// doVertexRequest performs a single Vertex AI rawPredict call against the given endpoint.
func (c *Client) doVertexRequest(ctx context.Context, endpoint string, body []byte) (string, *Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Endpoint: .../models/{MODEL}:rawPredict
	url := fmt.Sprintf("%s/%s:rawPredict", endpoint, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}

	token, err := c.tokenSource.Token()
	if err != nil {
		return "", nil, fmt.Errorf("llm: vertex auth: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", nil, c.statusError(resp.StatusCode, b)
	}

	return decodeAnthropicResponse(resp.Body)
}

// decodeAnthropicResponse parses the Anthropic Messages API response shape used
// by both the native Anthropic and Vertex AI providers.
func decodeAnthropicResponse(body io.Reader) (string, *Usage, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return "", nil, err
	}
	usage := &Usage{
		InputTokens:              out.Usage.InputTokens,
		OutputTokens:             out.Usage.OutputTokens,
		CacheCreationInputTokens: out.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     out.Usage.CacheReadInputTokens,
	}
	for _, block := range out.Content {
		if block.Type == "text" {
			return block.Text, usage, nil
		}
	}
	return "", usage, fmt.Errorf("llm: no text content in response")
}
