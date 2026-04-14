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

// ChatMessage is one turn in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Client calls either an OpenAI-compatible, Anthropic, or Vertex AI chat endpoint.
type Client struct {
	provider    string
	endpoint    string
	apiKey      string
	model       string
	timeout     time.Duration
	http        *http.Client
	tokenSource oauth2.TokenSource // for Vertex AI (ADC)
	maxTokens   int                // 0 → use default (maxTokens const)
}

// WithMaxTokens returns a shallow copy of the client with a custom max_tokens limit.
func (c *Client) WithMaxTokens(n int) *Client {
	c2 := *c
	c2.maxTokens = n
	return &c2
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
		provider: provider,
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		timeout:  timeout,
		http:     &http.Client{Timeout: timeout + 2*time.Second},
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

// Complete sends a chat completion request and returns the assistant's reply.
func (c *Client) Complete(ctx context.Context, messages []ChatMessage) (string, error) {
	switch c.provider {
	case "anthropic":
		return c.completeAnthropic(ctx, messages)
	case "vertex":
		return c.completeVertex(ctx, messages)
	default:
		return c.completeOpenAI(ctx, messages) // "openai", "ollama", "groq" use OpenAI-compatible API
	}
}

// ── OpenAI ────────────────────────────────────────────────────────────────────

func (c *Client) completeOpenAI(ctx context.Context, messages []ChatMessage) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":       c.model,
		"messages":    messages,
		"max_tokens":  c.effectiveMaxTokens(),
		"temperature": 0,
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", c.statusError(resp.StatusCode, b)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices in response")
	}
	return out.Choices[0].Message.Content, nil
}

// ── Anthropic ─────────────────────────────────────────────────────────────────

func (c *Client) completeAnthropic(ctx context.Context, messages []ChatMessage) (string, error) {
	// Anthropic's Messages API separates the system prompt from the conversation.
	// Extract the first system message (if any); the rest must be user/assistant.
	var system string
	var convo []ChatMessage
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
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
		reqBody["system"] = system
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", c.statusError(resp.StatusCode, b)
	}

	// Anthropic response: {"content": [{"type": "text", "text": "..."}], ...}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	for _, block := range out.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("llm: no text content in anthropic response")
}

// ── Vertex AI ────────────────────────────────────────────────────────────────

func (c *Client) completeVertex(ctx context.Context, messages []ChatMessage) (string, error) {
	if c.tokenSource == nil {
		return "", fmt.Errorf("llm: vertex provider requires application default credentials")
	}

	// Same request body as Anthropic Messages API.
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

	reqBody := map[string]any{
		"max_tokens":        c.effectiveMaxTokens(),
		"messages":          convo,
		"temperature":       0,
		"anthropic_version": vertexAnthropicVersion,
	}
	if system != "" {
		reqBody["system"] = system
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Endpoint: .../models/{MODEL}:rawPredict
	url := fmt.Sprintf("%s/%s:rawPredict", c.endpoint, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	token, err := c.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("llm: vertex auth: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", c.statusError(resp.StatusCode, b)
	}

	// Response format is the same as Anthropic Messages API.
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	for _, block := range out.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("llm: no text content in vertex response")
}
