// Package roles drives the four LLM-driven actors that exercise the
// runtime proxy: user-sim, responder, approver, and judge. Each role gets
// its own prompt and turn loop; together they form one scenario run.
//
// The roles speak directly to api.anthropic.com via a minimal HTTP client
// (we don't reuse internal/llm because the responder needs tool use and the
// transcript shape is different).
package roles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"

	// EnvAPIKey is the env var the harness reads for Claude credentials. If
	// unset, e2e tests skip — callers should check Skip() first.
	EnvAPIKey = "CLAWVISOR_E2E_ANTHROPIC_KEY"
)

// Skip reports whether the LLM-driven tests should be skipped because no
// Anthropic API key is configured. Smoke tests set their own paths.
func Skip() (string, bool) {
	if os.Getenv(EnvAPIKey) == "" {
		return "set " + EnvAPIKey + " to run LLM-driven scenarios", true
	}
	return "", false
}

// Client is a thin Anthropic Messages API client with tool-use support.
type Client struct {
	apiKey string
	model  string
	http   *http.Client
}

// NewClient builds a Client for the given model. Pass an empty model to use
// claude-sonnet-4-6 by default.
func NewClient(apiKey, model string) *Client {
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &Client{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 120 * time.Second},
	}
}

// Message is a single message in the Anthropic Messages format. Content is
// either a string (for plain user/assistant turns) or an array of content
// blocks (text, tool_use, tool_result).
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// ContentBlock is one entry in a multi-part message body.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	// Content for tool_result: a string or array of blocks. We keep it
	// loose because Anthropic accepts both.
	Content any  `json:"content,omitempty"`
	IsError bool `json:"is_error,omitempty"`
}

// Tool is one tool the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Request is the input to Send. System is the system prompt; Messages is
// the running transcript; Tools is the tool catalog.
type Request struct {
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens"`
}

// Response is the trimmed Anthropic response body the harness cares about.
type Response struct {
	StopReason string         `json:"stop_reason"`
	Content    []ContentBlock `json:"content"`
}

// Send issues one Messages call and returns the response.
func (c *Client) Send(ctx context.Context, req Request) (*Response, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("anthropic: api key is empty")
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	body := map[string]any{
		"model":      c.model,
		"max_tokens": req.MaxTokens,
		"messages":   req.Messages,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, string(respBody))
	}
	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("anthropic: decode: %w", err)
	}
	return &out, nil
}

// FirstText returns the first text block of the response, or empty string.
func (r *Response) FirstText() string {
	for _, b := range r.Content {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}
