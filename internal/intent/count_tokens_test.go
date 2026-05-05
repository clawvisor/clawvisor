package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLive_CountVerificationPromptTokens calls Anthropic's count_tokens
// endpoint to report exact token counts for the verification system prompt
// (strict and lenient variants). Used to size padding for the Haiku 4096-token
// cache threshold.
//
//	CLAWVISOR_LLM_API_KEY=sk-ant-... go test -run TestLive_CountVerificationPromptTokens -v ./internal/intent/
func TestLive_CountVerificationPromptTokens(t *testing.T) {
	apiKey := os.Getenv("CLAWVISOR_LLM_API_KEY")
	if apiKey == "" {
		t.Skip("CLAWVISOR_LLM_API_KEY not set")
	}
	model := os.Getenv("CLAWVISOR_LLM_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	endpoint := os.Getenv("CLAWVISOR_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1"
	}

	cases := []struct {
		name   string
		system string
	}{
		{"verification (strict)", verificationSystemPrompt},
		{"verification (lenient)", verificationSystemPrompt + lenientAddendum},
		{"chain context extraction", extractionSystemPrompt},
	}
	for _, c := range cases {
		n := countTokens(t, endpoint, apiKey, model, c.system)
		gap := 4096 - n
		t.Logf("%s: %d tokens (%d bytes) — gap to Haiku 4096 floor: %d tokens",
			c.name, n, len(c.system), gap)
	}
}

// countTokens hits POST /v1/messages/count_tokens and returns input_tokens.
// The endpoint accepts the same shape as /messages: model + system + messages.
func countTokens(t *testing.T, endpoint, apiKey, model, system string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"system":   system,
		"messages": []map[string]any{{"role": "user", "content": "."}},
	})
	req, err := http.NewRequest(http.MethodPost, endpoint+"/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, string(respBody))
	}
	return out.InputTokens
}
