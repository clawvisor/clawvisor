package llmproxy

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

const (
	ControlSyntheticHost = "clawvisor.local"
	ControlNotice        = "Clawvisor is available for permission management. To request permission for future tool use, call POST https://clawvisor.local/control/tasks. Task creation does not grant permission until the user approves it. For schemas and examples, call GET https://clawvisor.local/control/skill."
)

// InjectControlNotice adds a compact control-plane hint to the request context.
// The synthetic URL is rewritten from model-emitted tool calls before the tool
// runner sees it, so the prompt stays stable across local and public daemon URLs.
func InjectControlNotice(provider conversation.Provider, body []byte) ([]byte, bool, error) {
	if strings.Contains(string(body), "https://"+ControlSyntheticHost+"/control") {
		return body, false, nil
	}
	switch provider {
	case conversation.ProviderAnthropic:
		return injectAnthropicControlNotice(body)
	case conversation.ProviderOpenAI:
		return injectOpenAIControlNotice(body)
	default:
		return body, false, nil
	}
}

func injectAnthropicControlNotice(body []byte) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	sys, ok := raw["system"]
	if !ok || len(sys) == 0 || string(sys) == "null" {
		encoded, _ := json.Marshal(ControlNotice)
		raw["system"] = encoded
		return marshalInjected(raw)
	}
	var s string
	if err := json.Unmarshal(sys, &s); err == nil {
		encoded, _ := json.Marshal(appendNotice(s))
		raw["system"] = encoded
		return marshalInjected(raw)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(sys, &blocks); err == nil {
		blocks = append(blocks, map[string]any{"type": "text", "text": ControlNotice})
		encoded, err := json.Marshal(blocks)
		if err != nil {
			return nil, false, err
		}
		raw["system"] = encoded
		return marshalInjected(raw)
	}
	return body, false, nil
}

func injectOpenAIControlNotice(body []byte) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if messages, ok, err := injectOpenAIMessages(raw["messages"]); err != nil {
		return nil, false, err
	} else if ok {
		raw["messages"] = messages
		return marshalInjected(raw)
	}
	if instr, ok := raw["instructions"]; ok && len(instr) > 0 && string(instr) != "null" {
		var s string
		if err := json.Unmarshal(instr, &s); err != nil {
			return body, false, nil
		}
		encoded, _ := json.Marshal(appendNotice(s))
		raw["instructions"] = encoded
		return marshalInjected(raw)
	}
	encoded, _ := json.Marshal(ControlNotice)
	raw["instructions"] = encoded
	return marshalInjected(raw)
}

func marshalInjected(v any) ([]byte, bool, error) {
	out, err := json.Marshal(v)
	return out, err == nil, err
}

func injectOpenAIMessages(raw json.RawMessage) (json.RawMessage, bool, error) {
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
				messages[0]["content"] = appendNotice(s)
				out, err := json.Marshal(messages)
				return out, true, err
			}
		}
	}
	messages = append([]map[string]any{{"role": "system", "content": ControlNotice}}, messages...)
	out, err := json.Marshal(messages)
	return out, true, err
}

func appendNotice(existing string) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return ControlNotice
	}
	return existing + "\n\n" + ControlNotice
}

// RewriteControlToolUse redirects a model-emitted synthetic control URL to the
// daemon and injects caller auth. This path intentionally bypasses policy rules:
// agents must be able to ask Clawvisor for permission before permission exists.
func RewriteControlToolUse(t conversation.ToolUse, controlBaseURL string, callerToken string) ([]byte, inspector.Verdict, bool, error) {
	if strings.TrimSpace(controlBaseURL) == "" {
		return nil, inspector.Verdict{}, false, nil
	}
	v, ok := controlVerdictForToolUse(t)
	if !ok {
		return nil, inspector.Verdict{}, false, nil
	}
	opts := inspector.DefaultRewriteOpts(controlBaseURL)
	opts.CallerToken = callerToken
	rewritten, err := inspector.Rewrite(inspector.ToolUse{
		ID:    t.ID,
		Name:  t.Name,
		Input: t.Input,
	}, v, opts)
	return rewritten, v, true, err
}

func controlVerdictForToolUse(t conversation.ToolUse) (inspector.Verdict, bool) {
	if len(t.Input) == 0 {
		return inspector.Verdict{}, false
	}
	if u, ok := controlURLFromStructuredInput(t.Input); ok {
		return controlVerdict(u), true
	}
	if u, ok := controlURLFromCommandInput(t.Input); ok {
		return controlVerdict(u), true
	}
	return inspector.Verdict{}, false
}

func controlURLFromStructuredInput(in json.RawMessage) (*url.URL, bool) {
	var raw struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(in, &raw); err != nil || raw.URL == "" {
		return nil, false
	}
	return parseControlURL(raw.URL)
}

func controlURLFromCommandInput(in json.RawMessage) (*url.URL, bool) {
	var raw struct {
		Cmd     string `json:"cmd,omitempty"`
		Command string `json:"command,omitempty"`
	}
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, false
	}
	cmd := strings.TrimSpace(raw.Cmd)
	if cmd == "" {
		cmd = strings.TrimSpace(raw.Command)
	}
	if cmd == "" || !strings.HasPrefix(strings.ToLower(cmd), "curl ") {
		return nil, false
	}
	if hasControlRewriteUnsafeShell(cmd) {
		return nil, false
	}
	match := controlURLRE.FindString(cmd)
	if match == "" {
		return nil, false
	}
	return parseControlURL(match)
}

func parseControlURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}
	if !strings.EqualFold(u.Hostname(), ControlSyntheticHost) {
		return nil, false
	}
	if u.Path != "/control" && !strings.HasPrefix(u.Path, "/control/") {
		return nil, false
	}
	return u, true
}

func controlVerdict(u *url.URL) inspector.Verdict {
	method := "GET"
	if strings.HasSuffix(u.Path, "/tasks") {
		method = "POST"
	}
	return inspector.Verdict{
		IsAPICall: true,
		Method:    method,
		Host:      u.Hostname(),
		Path:      u.RequestURI(),
		Source:    inspector.SourceDeterministic,
		Reason:    "synthetic Clawvisor control endpoint",
	}
}

func hasControlRewriteUnsafeShell(cmd string) bool {
	for _, c := range cmd {
		switch c {
		case '\n', '|', ';', '&', '`', '$', '<', '>':
			return true
		}
	}
	return false
}

var controlURLRE = regexp.MustCompile(`https?://clawvisor\.local(?::[0-9]+)?/control[^\s'"]*`)
