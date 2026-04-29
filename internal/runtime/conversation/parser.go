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
	host := strings.ToLower(hostFromRequest(req))
	if host != "api.openai.com" {
		return false
	}
	return strings.HasPrefix(req.URL.Path, "/v1/responses") || strings.HasPrefix(req.URL.Path, "/v1/chat/completions")
}

func (OpenAIParser) ParseRequest(body []byte) ([]Turn, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if raw, ok := probe["messages"]; ok {
		return parseOpenAIMessages(raw)
	}
	if raw, ok := probe["input"]; ok {
		return parseOpenAIInput(raw)
	}
	return nil, nil
}

func parseOpenAIMessages(raw json.RawMessage) ([]Turn, error) {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		content := flattenOpenAIContent(m.Content)
		if content == "" {
			continue
		}
		role := RoleUser
		switch m.Role {
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
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
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
