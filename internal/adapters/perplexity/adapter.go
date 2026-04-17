// Package perplexity implements the Go action override for the Perplexity
// chat action. The YAML definition handles the /search action; this package
// handles /chat/completions because the API requires building a messages
// array [{role, content}] that cannot be expressed declaratively in YAML.
package perplexity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

const baseURL = "https://api.perplexity.ai"

// Adapter handles Perplexity actions that require Go logic.
type Adapter struct{}

// New returns a new Perplexity Adapter.
func New() *Adapter { return &Adapter{} }

// Execute dispatches to the appropriate action handler.
func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	switch req.Action {
	case "chat":
		return chatAction(ctx, req)
	default:
		return nil, fmt.Errorf("perplexity: unsupported action %q", req.Action)
	}
}

// chatAction handles POST /chat/completions by wrapping the query string
// into the messages array format Perplexity requires.
func chatAction(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	// req.Credential is []byte from the vault
	if err != nil {
		return nil, fmt.Errorf("perplexity: %w", err)
	}

	query, _ := req.Params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("perplexity: query is required")
	}

	model := "sonar"
	if m, ok := req.Params["model"].(string); ok && m != "" {
		model = m
	}

	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": query},
		},
	}

	if sm, ok := req.Params["search_mode"].(string); ok && sm != "" {
		body["search_mode"] = sm
	}
	if rf, ok := req.Params["search_recency_filter"].(string); ok && rf != "" {
		body["search_recency_filter"] = rf
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("perplexity: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("perplexity: create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("perplexity: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("perplexity: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("perplexity: API error %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Citations []string `json:"citations"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("perplexity: parse response: %w", err)
	}

	answer := ""
	if len(result.Choices) > 0 {
		answer = result.Choices[0].Message.Content
	}

	// Truncate very long answers for the summary.
	summary := answer
	if len(summary) > 300 {
		summary = summary[:300] + "…"
	}

	data := map[string]any{
		"answer":    answer,
		"citations": result.Citations,
		"model":     model,
	}

	return &adapters.Result{
		Data:    data,
		Summary: summary,
	}, nil
}

// extractToken pulls the Bearer token from the raw credential bytes.
func extractToken(credBytes []byte) (string, error) {
	var cred struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("parsing credential: %w", err)
	}
	token := cred.Token
	if token == "" {
		token = cred.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("credential missing token")
	}
	return token, nil
}
