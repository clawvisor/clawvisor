package taskrisk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive_CountRiskAssessmentPromptTokens calls Anthropic's count_tokens
// endpoint to report the exact token count of the risk assessment system
// prompt. The constant has a "%s" placeholder for adapter action context;
// we measure the static portion (with empty action context) only.
//
//	CLAWVISOR_LLM_API_KEY=sk-ant-... go test -run TestLive_CountRiskAssessmentPromptTokens -v ./internal/taskrisk/
func TestLive_CountRiskAssessmentPromptTokens(t *testing.T) {
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

	// Render with an empty action context to measure the static prefix.
	system := fmt.Sprintf(riskAssessmentSystemPrompt, "")
	// Defensive: confirm the format directive resolved (no leftover %s literals).
	if strings.Contains(system, "%!s") {
		t.Fatalf("riskAssessmentSystemPrompt has unexpected fmt verbs")
	}

	n := countTokens(t, endpoint, apiKey, model, system)
	gap := 4096 - n
	t.Logf("risk assessment (static, empty action context): %d tokens (%d bytes) — gap to Haiku 4096 floor: %d tokens",
		n, len(system), gap)
}

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
