package llmproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	ControlToolProtocol = "cv_protocol"
	ControlToolTask     = "cv_task"
)

func ControlNotice() string {
	return "Clawvisor synthetic tools are available. Call cv_protocol for task-request instructions and schema. Call cv_task to request user approval for future tool use. Do not use curl, localhost, loopback, or clawvisor.local for Clawvisor control; there is no HTTP compatibility fallback."
}

// InjectControlTools adds Clawvisor's synthetic control tools and a compact
// hint to the request context. The tools are executed inside proxy-lite before
// the harness sees them.
func InjectControlTools(provider conversation.Provider, routePath string, body []byte) ([]byte, bool, error) {
	notice := ControlNotice()
	switch provider {
	case conversation.ProviderAnthropic:
		return injectAnthropicControlTools(body, notice)
	case conversation.ProviderOpenAI:
		return injectOpenAIControlTools(body, routePath, notice)
	default:
		return body, false, nil
	}
}

func injectAnthropicControlTools(body []byte, notice string) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	changed := false
	tools, toolsChanged, err := injectAnthropicToolDefs(raw["tools"])
	if err != nil {
		return nil, false, err
	}
	if toolsChanged {
		raw["tools"] = tools
		changed = true
	}
	sys, ok := raw["system"]
	if !ok || len(sys) == 0 || string(sys) == "null" {
		encoded, _ := json.Marshal(notice)
		raw["system"] = encoded
		changed = true
		return marshalInjectedIfChanged(raw, changed)
	}
	var s string
	if err := json.Unmarshal(sys, &s); err == nil {
		if !strings.Contains(s, ControlToolTask) {
			encoded, _ := json.Marshal(appendNotice(s, notice))
			raw["system"] = encoded
			changed = true
		}
		return marshalInjectedIfChanged(raw, changed)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(sys, &blocks); err == nil {
		if !systemBlocksContain(blocks, ControlToolTask) {
			blocks = append(blocks, map[string]any{"type": "text", "text": notice})
			encoded, err := json.Marshal(blocks)
			if err != nil {
				return nil, false, err
			}
			raw["system"] = encoded
			changed = true
		}
		return marshalInjectedIfChanged(raw, changed)
	}
	return marshalInjectedIfChanged(raw, changed)
}

func injectOpenAIControlTools(body []byte, routePath string, notice string) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	changed := false
	tools, toolsChanged, err := injectOpenAIToolDefs(raw["tools"], routePath)
	if err != nil {
		return nil, false, err
	}
	if toolsChanged {
		raw["tools"] = tools
		changed = true
	}
	if _, hasMessages := raw["messages"]; hasMessages {
		if messages, ok, err := injectOpenAIMessages(raw["messages"], notice); err != nil {
			return nil, false, err
		} else if ok {
			raw["messages"] = messages
			changed = true
		}
		return marshalInjectedIfChanged(raw, changed)
	}
	if instr, ok := raw["instructions"]; ok && len(instr) > 0 && string(instr) != "null" {
		var s string
		if err := json.Unmarshal(instr, &s); err != nil {
			return marshalInjectedIfChanged(raw, changed)
		}
		if !strings.Contains(s, ControlToolTask) {
			encoded, _ := json.Marshal(appendNotice(s, notice))
			raw["instructions"] = encoded
			changed = true
		}
		return marshalInjectedIfChanged(raw, changed)
	}
	encoded, _ := json.Marshal(notice)
	raw["instructions"] = encoded
	changed = true
	return marshalInjectedIfChanged(raw, changed)
}

func marshalInjectedIfChanged(v any, changed bool) ([]byte, bool, error) {
	if !changed {
		return nil, false, nil
	}
	out, err := json.Marshal(v)
	return out, err == nil, err
}

func injectAnthropicToolDefs(raw json.RawMessage) (json.RawMessage, bool, error) {
	var tools []map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, false, err
		}
	}
	changed := false
	if !toolListHasName(tools, ControlToolProtocol) {
		tools = append(tools, anthropicControlTool(ControlToolProtocol, "Return Clawvisor task-request protocol instructions and schemas.", controlProtocolInputSchema()))
		changed = true
	}
	if !toolListHasName(tools, ControlToolTask) {
		tools = append(tools, anthropicControlTool(ControlToolTask, "Create a Clawvisor task approval request for future tool use.", controlTaskInputSchema()))
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	out, err := json.Marshal(tools)
	return out, true, err
}

func injectOpenAIToolDefs(raw json.RawMessage, routePath string) (json.RawMessage, bool, error) {
	var tools []map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, false, err
		}
	}
	changed := false
	chatShape := strings.Contains(routePath, "/chat/completions")
	if !toolListHasName(tools, ControlToolProtocol) {
		tools = append(tools, openAIControlTool(ControlToolProtocol, "Return Clawvisor task-request protocol instructions and schemas.", controlProtocolInputSchema(), chatShape))
		changed = true
	}
	if !toolListHasName(tools, ControlToolTask) {
		tools = append(tools, openAIControlTool(ControlToolTask, "Create a Clawvisor task approval request for future tool use.", controlTaskInputSchema(), chatShape))
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	out, err := json.Marshal(tools)
	return out, true, err
}

func anthropicControlTool(name, description string, schema map[string]any) map[string]any {
	return map[string]any{
		"name":         name,
		"description":  description,
		"input_schema": schema,
	}
}

func openAIControlTool(name, description string, schema map[string]any, chatShape bool) map[string]any {
	if chatShape {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  schema,
			},
		}
	}
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"parameters":  schema,
		"strict":      false,
	}
}

func controlProtocolInputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"topic": map[string]any{
				"type":        "string",
				"description": "Optional protocol topic.",
				"enum":        []string{"tasks"},
			},
		},
	}
}

func controlTaskInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"purpose": map[string]any{
				"type":        "string",
				"description": "Briefly explain the user-visible work that needs approval.",
			},
			"expected_tools_json": map[string]any{
				"type":        "array",
				"description": "Harness tool uses that should be allowed if the user approves.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool_name": map[string]any{"type": "string"},
						"why":       map[string]any{"type": "string"},
						"input_regex": map[string]any{
							"type":        "string",
							"description": "Optional regex restricting allowed tool input.",
						},
					},
					"required":             []string{"tool_name", "why"},
					"additionalProperties": true,
				},
			},
			"intent_verification_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"strict", "lenient", "off"},
				"description": "Defaults to strict.",
			},
			"expires_in_seconds": map[string]any{
				"type":        "integer",
				"description": "Requested task TTL in seconds.",
			},
		},
		"required":             []string{"purpose", "expected_tools_json"},
		"additionalProperties": true,
	}
}

func toolListHasName(tools []map[string]any, name string) bool {
	for _, tool := range tools {
		if s, _ := tool["name"].(string); s == name {
			return true
		}
		if fn, _ := tool["function"].(map[string]any); fn != nil {
			if s, _ := fn["name"].(string); s == name {
				return true
			}
		}
	}
	return false
}

func systemBlocksContain(blocks []map[string]any, needle string) bool {
	for _, block := range blocks {
		if s, _ := block["text"].(string); strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func injectOpenAIMessages(raw json.RawMessage, notice string) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, false, err
	}
	if len(messages) > 0 {
		role, _ := messages[0]["role"].(string)
		if role == "system" || role == "developer" {
			if s, ok := messages[0]["content"].(string); ok {
				if strings.Contains(s, ControlToolTask) {
					return nil, false, nil
				}
				messages[0]["content"] = appendNotice(s, notice)
				out, err := json.Marshal(messages)
				return out, true, err
			}
		}
	}
	messages = append([]map[string]any{{"role": "system", "content": notice}}, messages...)
	out, err := json.Marshal(messages)
	return out, true, err
}

func appendNotice(existing, notice string) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return notice
	}
	return existing + "\n\n" + notice
}

type ControlExecutor interface {
	ExecuteControl(ctx context.Context, req ControlExecutionRequest) (ControlExecutionResponse, error)
}

type ControlExecutionRequest struct {
	Agent    *store.Agent
	ToolName string
	Body     []byte
}

type ControlExecutionResponse struct {
	StatusCode   int
	ContentType  string
	Body         []byte
	ErrorMessage string
}

type ControlCall struct {
	ToolName string
	Body     []byte
	Verdict  inspector.Verdict
}

func ParseControlToolUse(t conversation.ToolUse) (ControlCall, bool) {
	name := strings.TrimSpace(t.Name)
	if name != ControlToolProtocol && name != ControlToolTask {
		return ControlCall{}, false
	}
	body := t.Input
	if len(body) == 0 || string(body) == "null" {
		body = []byte(`{}`)
	}
	return ControlCall{
		ToolName: name,
		Body:     body,
		Verdict:  controlVerdictForTool(name),
	}, true
}

func FormatControlExecutionResponse(resp ControlExecutionResponse) string {
	status := resp.StatusCode
	if status == 0 {
		status = 500
	}
	if resp.ErrorMessage != "" {
		return fmt.Sprintf("Clawvisor synthetic tool failed.\n\nHTTP %d\n%s", status, resp.ErrorMessage)
	}
	body := strings.TrimSpace(string(resp.Body))
	if body == "" {
		return fmt.Sprintf("Clawvisor synthetic tool handled.\n\nHTTP %d", status)
	}
	return fmt.Sprintf("Clawvisor synthetic tool handled.\n\nHTTP %d\n```json\n%s\n```", status, body)
}

func controlVerdictForTool(name string) inspector.Verdict {
	method := "CALL"
	if name == ControlToolTask {
		method = "CREATE"
	}
	return inspector.Verdict{
		IsAPICall: true,
		Method:    method,
		Host:      "clawvisor.control",
		Path:      "/" + name,
		Source:    inspector.SourceDeterministic,
		Reason:    "synthetic Clawvisor control tool",
	}
}
