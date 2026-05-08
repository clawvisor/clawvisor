package inspector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicValidator calls Haiku to classify a triggered tool_use. It is
// the **authorization boundary** for shapes the deterministic parser
// cannot decide; downstream BoundaryCheck must still bind the verdict's
// host to the placeholder's bound-service allowlist.
//
// The prompt is content-addressed (SHA-256). Audit rows record the SHA at
// decision time so a prompt change is a forensic event, not a silent
// behavior shift.
//
// NOTE: prompt draft for review. Iterate on it together before locking
// the calibration suite.
type AnthropicValidator struct {
	APIKey string
	Model  string // default: "claude-haiku-4-5"
	HTTP   *http.Client
	// PromptOverride lets tests inject a deterministic prompt. Production
	// uses the embedded ValidatorPrompt below.
	PromptOverride string
}

// ValidatorPrompt is the system prompt sent to Haiku for shape
// classification. Keep it terse and structured-output-friendly.
//
// DRAFT — not locked. Reviewed jointly with the user before the
// calibration suite is committed.
const ValidatorPrompt = `You are a request-shape classifier for a security
proxy. Given a single tool_use payload that contains an "autovault_…"
shadow placeholder, decide whether it represents a credentialed HTTP API
call that should be mediated.

Return ONLY a JSON object with this exact shape:
{
  "is_api_call": true|false,
  "ambiguous": true|false,
  "method": "GET"|"POST"|"PUT"|"PATCH"|"DELETE"|"",
  "host": "<lowercased hostname>",
  "path": "<request URI including query>",
  "credential_locations": [
    {"kind":"header","name":"Authorization","scheme":"Bearer"}
  ],
  "reason": "<short explanation>"
}

Rules:
1. If the placeholder appears in a log line, comment, echo, or any
   non-credential context, return {"is_api_call": false, "ambiguous": false}.
2. If you cannot confidently determine host, method, or credential location,
   set {"ambiguous": true} and the proxy will fail closed.
3. Never invent fields. If you don't see something, leave it empty.
4. Do not include explanation text outside the JSON object.`

// PromptSHA returns the SHA-256 of the prompt currently in use. Audit
// rows MUST include this value so a prompt change is forensically visible.
func (v *AnthropicValidator) PromptSHA() string {
	prompt := v.PromptOverride
	if prompt == "" {
		prompt = ValidatorPrompt
	}
	h := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(h[:])
}

type anthropicValidatorResponse struct {
	IsAPICall           bool                 `json:"is_api_call"`
	Ambiguous           bool                 `json:"ambiguous"`
	Method              string               `json:"method"`
	Host                string               `json:"host"`
	Path                string               `json:"path"`
	CredentialLocations []CredentialLocation `json:"credential_locations"`
	Reason              string               `json:"reason"`
}

// Validate implements Validator. Returns ambiguous=true on any error so
// the rewriter fails closed rather than acting on a half-baked verdict.
func (v *AnthropicValidator) Validate(ctx context.Context, t ToolUse) (Verdict, error) {
	if v == nil || v.APIKey == "" {
		return Verdict{Ambiguous: true, Reason: "validator not configured"}, nil
	}
	model := v.Model
	if model == "" {
		model = "claude-haiku-4-5"
	}
	prompt := v.PromptOverride
	if prompt == "" {
		prompt = ValidatorPrompt
	}
	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	userMessage := fmt.Sprintf(`tool_name: %q
tool_input: %s`, t.Name, string(t.Input))

	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 256,
		"system":     prompt,
		"messages": []map[string]any{
			{"role": "user", "content": userMessage},
		},
	})
	if err != nil {
		return Verdict{Ambiguous: true, Reason: "encode error: " + err.Error()}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Verdict{Ambiguous: true, Reason: "request error: " + err.Error()}, nil
	}
	req.Header.Set("x-api-key", v.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Verdict{Ambiguous: true, Reason: "transport error: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return Verdict{Ambiguous: true, Reason: "validator http " + resp.Status}, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{Ambiguous: true, Reason: "read error: " + err.Error()}, nil
	}

	verdict, err := extractAnthropicVerdict(raw)
	if err != nil {
		return Verdict{Ambiguous: true, Reason: "parse error: " + err.Error()}, nil
	}
	return verdict, nil
}

// extractAnthropicVerdict parses the Anthropic /v1/messages JSON response
// and pulls the JSON object the model emitted in its first text content
// block.
func extractAnthropicVerdict(raw []byte) (Verdict, error) {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Verdict{}, fmt.Errorf("response envelope: %w", err)
	}
	for _, c := range resp.Content {
		if c.Type != "text" {
			continue
		}
		// The model may wrap output in code fences; trim them.
		text := strings.TrimSpace(c.Text)
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
		var parsed anthropicValidatorResponse
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			continue
		}
		return Verdict{
			IsAPICall:           parsed.IsAPICall,
			Ambiguous:           parsed.Ambiguous,
			Method:              canonicalMethod(parsed.Method),
			Host:                strings.ToLower(strings.TrimSpace(parsed.Host)),
			Path:                parsed.Path,
			CredentialLocations: parsed.CredentialLocations,
			Reason:              parsed.Reason,
		}, nil
	}
	return Verdict{}, errors.New("no JSON content block in response")
}
