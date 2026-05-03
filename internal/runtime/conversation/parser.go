package conversation

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

type Parser interface {
	Name() Provider
	Matches(req *http.Request) bool
	ParseRequest(body []byte) ([]Turn, error)
}

type Registry struct {
	parsers []Parser
}

func DefaultRegistry() *Registry {
	return &Registry{parsers: []Parser{
		&AnthropicParser{},
		&OpenAIParser{},
	}}
}

func (r *Registry) Match(req *http.Request) Parser {
	for _, p := range r.parsers {
		if p.Matches(req) {
			return p
		}
	}
	return nil
}

type AnthropicParser struct{}

func (AnthropicParser) Name() Provider { return ProviderAnthropic }

func (AnthropicParser) Matches(req *http.Request) bool {
	host := strings.ToLower(hostFromRequest(req))
	return host == "api.anthropic.com" && strings.HasPrefix(req.URL.Path, "/v1/messages")
}

type anthropicRequest struct {
	Messages []anthropicMessage `json:"messages"`
	System   json.RawMessage    `json:"system,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

const maxAnthropicContentDepth = 16

func (AnthropicParser) ParseRequest(body []byte) ([]Turn, error) {
	var r anthropicRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(r.Messages)+1)
	if sys := flattenAnthropicContent(r.System, 0); sys != "" {
		out = append(out, Turn{Role: RoleSystem, Content: sys})
	}

	toolNames := map[string]string{}
	for _, m := range r.Messages {
		collectAnthropicToolUseNames(m.Content, 0, toolNames)
	}

	for _, m := range r.Messages {
		content := flattenAnthropicContent(m.Content, 0)
		if content == "" {
			continue
		}
		role := RoleUser
		var toolName string
		switch m.Role {
		case "assistant":
			role = RoleAssistant
		case "tool":
			role = RoleTool
		case "user":
			if ids, ok := anthropicToolResultIDs(m.Content); ok {
				role = RoleTool
				toolName = joinToolNames(ids, toolNames)
			}
		}
		out = append(out, Turn{Role: role, Content: content, ToolName: toolName})
	}
	return out, nil
}

func anthropicToolResultIDs(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return nil, false
		}
		ids = append(ids, b.ToolUseID)
	}
	return ids, true
}

func collectAnthropicToolUseNames(raw json.RawMessage, depth int, out map[string]string) {
	if len(raw) == 0 || depth >= maxAnthropicContentDepth {
		return
	}
	var blocks []struct {
		Type    string          `json:"type"`
		ID      string          `json:"id,omitempty"`
		Name    string          `json:"name,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" && b.Name != "" {
			out[b.ID] = b.Name
		}
		if len(b.Content) > 0 {
			collectAnthropicToolUseNames(b.Content, depth+1, out)
		}
	}
}

func joinToolNames(ids []string, names map[string]string) string {
	seen := map[string]struct{}{}
	var got []string
	for _, id := range ids {
		n := names[id]
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		got = append(got, n)
	}
	return strings.Join(got, ", ")
}

func flattenAnthropicContent(raw json.RawMessage, depth int) string {
	if len(raw) == 0 || depth >= maxAnthropicContentDepth {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
		Name    string          `json:"name,omitempty"`
		Input   json.RawMessage `json:"input,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				b.WriteString(blk.Text)
				b.WriteByte('\n')
			case "tool_use":
				b.WriteString("<tool_use name=")
				b.WriteString(blk.Name)
				if len(blk.Input) > 0 {
					b.WriteString(" input=")
					b.Write(blk.Input)
				}
				b.WriteByte('>')
				b.WriteByte('\n')
			case "tool_result":
				b.WriteString(flattenAnthropicContent(blk.Content, depth+1))
				b.WriteByte('\n')
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

type OpenAIParser struct{}

func (OpenAIParser) Name() Provider { return ProviderOpenAI }

func (OpenAIParser) Matches(req *http.Request) bool {
	return matchOpenAIEndpoint(req)
}

func (OpenAIParser) ParseRequest(body []byte) ([]Turn, error) {
	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(req.Messages)+1)
	if req.Instructions != "" {
		out = append(out, Turn{Role: RoleSystem, Content: req.Instructions})
	}
	if len(req.Messages) > 0 {
		turns, err := parseOpenAIMessages(req.Messages)
		if err != nil {
			return nil, err
		}
		out = append(out, turns...)
	}
	if len(req.Input) > 0 {
		if turns, ok := parseOpenAIResponsesInput(req.Input); ok {
			out = append(out, turns...)
		} else if turns, err := parseOpenAIInput(req.Input); err == nil {
			out = append(out, turns...)
		}
	}
	return out, nil
}

type openAIRequest struct {
	Messages     []openAIMessage  `json:"messages"`
	Input        json.RawMessage  `json:"input,omitempty"`
	Instructions string           `json:"instructions,omitempty"`
}

type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type openAIInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

func matchOpenAIEndpoint(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(hostFromRequest(req))
	switch host {
	case "api.openai.com":
		return strings.HasPrefix(req.URL.Path, "/v1/responses") || strings.HasPrefix(req.URL.Path, "/v1/chat/completions")
	case "chatgpt.com":
		return strings.HasPrefix(req.URL.Path, "/backend-api/codex/responses")
	default:
		return false
	}
}

func parseOpenAIMessages(msgs []openAIMessage) ([]Turn, error) {
	out := make([]Turn, 0, len(msgs))
	toolNames := make(map[string]string)
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		collectOpenAIChatToolCallNames(m.Content, toolNames)
	}
	for _, m := range msgs {
		content := flattenOpenAIContent(m.Content)
		if content == "" {
			continue
		}
		role := RoleUser
		var toolName string
		switch m.Role {
		case "assistant":
			role = RoleAssistant
		case "tool":
			role = RoleTool
			toolName = toolNames[m.ToolCallID]
		case "system", "developer":
			role = RoleSystem
		}
		out = append(out, Turn{Role: role, Content: content, ToolName: toolName})
	}
	return out, nil
}

func parseOpenAIInput(raw json.RawMessage) ([]Turn, error) {
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Type    string          `json:"type"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(items))
	for _, item := range items {
		if item.Type != "" && item.Type != "message" {
			continue
		}
		content := flattenOpenAIContent(item.Content)
		if content == "" {
			continue
		}
		role := RoleUser
		switch item.Role {
		case "assistant":
			role = RoleAssistant
		case "tool":
			role = RoleTool
		case "system", "developer":
			role = RoleSystem
		}
		out = append(out, Turn{Role: role, Content: content})
	}
	return out, nil
}

func parseOpenAIResponsesInput(raw json.RawMessage) ([]Turn, bool) {
	var items []openAIInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	known := false
	for _, item := range items {
		if isKnownOpenAIResponsesItem(item.Type) {
			known = true
			break
		}
	}
	if !known {
		return nil, false
	}
	out := make([]Turn, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "message":
			content := flattenOpenAIContent(item.Content)
			if content == "" {
				continue
			}
			role := RoleUser
			switch item.Role {
			case "assistant":
				role = RoleAssistant
			case "tool":
				role = RoleTool
			case "system", "developer":
				role = RoleSystem
			}
			out = append(out, Turn{Role: role, Content: content})
		case "function_call":
			var b strings.Builder
			b.WriteString("<tool_use name=")
			b.WriteString(item.Name)
			if args := unwrapOpenAIArguments(item.Arguments); args != "" {
				b.WriteString(" input=")
				b.WriteString(args)
			}
			b.WriteByte('>')
			out = append(out, Turn{Role: RoleAssistant, Content: b.String(), ToolName: item.Name})
		case "function_call_output":
			content := flattenOpenAIContent(item.Output)
			if content == "" {
				content = strings.TrimSpace(string(item.Output))
			}
			if content == "" {
				continue
			}
			out = append(out, Turn{Role: RoleTool, Content: content})
		}
	}
	return out, true
}

func isKnownOpenAIResponsesItem(t string) bool {
	switch t {
	case "message", "function_call", "function_call_output":
		return true
	default:
		return false
	}
}

func unwrapOpenAIArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var wrapped string
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		return strings.TrimSpace(wrapped)
	}
	return strings.TrimSpace(string(raw))
}

func flattenOpenAIContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
		Name string `json:"name,omitempty"`
		ID   string `json:"id,omitempty"`
		// Chat Completions tool-calls can arrive embedded in assistant content
		// in some compatibility surfaces.
		Input     json.RawMessage `json:"input,omitempty"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			switch blk.Type {
			case "text", "input_text", "output_text":
				if blk.Text == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			case "tool_use", "function_call":
				if blk.Name == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString("<tool_use name=")
				b.WriteString(blk.Name)
				args := unwrapOpenAIArguments(blk.Arguments)
				if args == "" && len(blk.Input) > 0 {
					args = strings.TrimSpace(string(blk.Input))
				}
				if args != "" {
					b.WriteString(" input=")
					b.WriteString(args)
				}
				b.WriteByte('>')
			}
		}
		return b.String()
	}
	return ""
}

func collectOpenAIChatToolCallNames(raw json.RawMessage, out map[string]string) {
	if len(raw) == 0 {
		return
	}
	var direct struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil && len(direct.ToolCalls) > 0 {
		for _, tc := range direct.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				out[tc.ID] = tc.Function.Name
			}
		}
		return
	}
	var blocks []struct {
		Type      string `json:"type"`
		ID        string `json:"id,omitempty"`
		Name      string `json:"name,omitempty"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return
	}
	for _, blk := range blocks {
		if blk.Type == "function_call" && blk.ID != "" && blk.Name != "" {
			out[blk.ID] = blk.Name
		}
		for _, tc := range blk.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				out[tc.ID] = tc.Function.Name
			}
		}
	}
}

func hostFromRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil && req.URL.Host != "" {
		return req.URL.Hostname()
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
