package filters

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/llm"
)

const filterSystemPrompt = `You are a data filter for an AI agent access control gateway.

Your job is to apply a filter instruction to a structured JSON result, then return the
filtered result in the SAME JSON schema as the input.

Rules you must follow:
- You may ONLY remove or redact items — never add, modify, or invent data
- If an item is ambiguous, keep it (err on the side of inclusion)
- If the entire result is empty after filtering, return an empty list/object in the same schema
- If you cannot apply the filter without violating these rules, return the data unchanged
  and set "applied" to false in your response

Respond ONLY with a JSON object on a single line, no markdown:
  {"applied": true,  "result": <filtered data matching input schema>}
  {"applied": false, "result": <original data unchanged>, "reason": "<why filter wasn't applied>"}`

// MakeLLMFilterFn creates a SemanticApplyFn backed by the given LLM client.
func MakeLLMFilterFn(client *llm.Client) SemanticApplyFn {
	return func(ctx context.Context, instruction string, result *adapters.Result) (*adapters.Result, bool) {
		data, _ := json.Marshal(result.Data)
		userMsg := "Filter instruction: " + instruction + "\n\nData to filter:\n" + string(data)

		messages := []llm.ChatMessage{
			{Role: "system", Content: filterSystemPrompt},
			{Role: "user", Content: userMsg},
		}

		raw, err := client.Complete(ctx, messages)
		if err != nil {
			return result, false
		}

		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)

		var out struct {
			Applied bool            `json:"applied"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return result, false
		}
		if !out.Applied || out.Result == nil {
			return result, false
		}

		var filtered any
		if err := json.Unmarshal(out.Result, &filtered); err != nil {
			return result, false
		}
		return &adapters.Result{
			Summary: result.Summary,
			Data:    filtered,
		}, true
	}
}
