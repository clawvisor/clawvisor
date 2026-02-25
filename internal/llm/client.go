// Package llm provides a thin HTTP client for LLM chat completions.
// Supports OpenAI-compatible endpoints (OpenAI, Groq, Ollama, …) and
// Anthropic's native Messages API.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/config"
)

const anthropicVersion = "2023-06-01"

// maxTokens is the upper bound sent on every request.
// All use-cases (safety: ~50 tokens, conflicts: ~256, policy YAML: ~600) fit within 1024.
const maxTokens = 1024

// ChatMessage is one turn in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Client calls either an OpenAI-compatible or Anthropic chat endpoint.
type Client struct {
	provider string
	endpoint string
	apiKey   string
	model    string
	timeout  time.Duration
	http     *http.Client
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
	return &Client{
		provider: provider,
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		timeout:  timeout,
		http:     &http.Client{Timeout: timeout + 2*time.Second},
	}
}

// Complete sends a chat completion request and returns the assistant's reply.
func (c *Client) Complete(ctx context.Context, messages []ChatMessage) (string, error) {
	if c.provider == "anthropic" {
		return c.completeAnthropic(ctx, messages)
	}
	return c.completeOpenAI(ctx, messages)
}

// ── OpenAI ────────────────────────────────────────────────────────────────────

func (c *Client) completeOpenAI(ctx context.Context, messages []ChatMessage) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":       c.model,
		"messages":    messages,
		"max_tokens":  maxTokens,
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("llm: status %d: %s", resp.StatusCode, b)
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
		"max_tokens":  maxTokens,
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
		return "", fmt.Errorf("llm: status %d: %s", resp.StatusCode, b)
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
