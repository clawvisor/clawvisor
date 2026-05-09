package llmproxy

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

const (
	ControlSyntheticHost = "clawvisor.local"
)

func ControlNotice(controlBaseURL string) string {
	controlBaseURL = strings.TrimRight(strings.TrimSpace(controlBaseURL), "/")
	docsURL := "https://" + ControlSyntheticHost + "/control/skill"
	if controlBaseURL != "" {
		docsURL = controlBaseURL + "/control/skill"
	}
	return "Clawvisor is available for permission management. For schemas and examples, call GET " + docsURL + ". To request permission for future tool use, emit a tool call that POSTs to https://clawvisor.local/control/tasks; Clawvisor rewrites that synthetic URL to the configured proxy endpoint before the shell runs it. Task creation does not grant permission until the user approves it."
}

// InjectControlNotice adds a compact control-plane hint to the request context.
// The synthetic URL is rewritten from model-emitted tool calls before the tool
// runner sees it, so the prompt stays stable across local and public daemon URLs.
func InjectControlNotice(provider conversation.Provider, body []byte, controlBaseURL string) ([]byte, bool, error) {
	if strings.Contains(string(body), "https://"+ControlSyntheticHost+"/control") {
		return body, false, nil
	}
	notice := ControlNotice(controlBaseURL)
	switch provider {
	case conversation.ProviderAnthropic:
		return injectAnthropicControlNotice(body, notice)
	case conversation.ProviderOpenAI:
		return injectOpenAIControlNotice(body, notice)
	default:
		return body, false, nil
	}
}

func injectAnthropicControlNotice(body []byte, notice string) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	sys, ok := raw["system"]
	if !ok || len(sys) == 0 || string(sys) == "null" {
		encoded, _ := json.Marshal(notice)
		raw["system"] = encoded
		return marshalInjected(raw)
	}
	var s string
	if err := json.Unmarshal(sys, &s); err == nil {
		encoded, _ := json.Marshal(appendNotice(s, notice))
		raw["system"] = encoded
		return marshalInjected(raw)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(sys, &blocks); err == nil {
		blocks = append(blocks, map[string]any{"type": "text", "text": notice})
		encoded, err := json.Marshal(blocks)
		if err != nil {
			return nil, false, err
		}
		raw["system"] = encoded
		return marshalInjected(raw)
	}
	return body, false, nil
}

func injectOpenAIControlNotice(body []byte, notice string) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	if messages, ok, err := injectOpenAIMessages(raw["messages"], notice); err != nil {
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
		encoded, _ := json.Marshal(appendNotice(s, notice))
		raw["instructions"] = encoded
		return marshalInjected(raw)
	}
	encoded, _ := json.Marshal(notice)
	raw["instructions"] = encoded
	return marshalInjected(raw)
}

func marshalInjected(v any) ([]byte, bool, error) {
	out, err := json.Marshal(v)
	return out, err == nil, err
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

type ControlCall struct {
	Method  string
	URL     *url.URL
	Path    string
	Body    []byte
	Verdict inspector.Verdict
}

func ParseControlToolUse(t conversation.ToolUse) (ControlCall, bool) {
	u, method, _, ok := controlCallParts(t)
	if !ok {
		return ControlCall{}, false
	}
	if method == "" {
		method = controlMethodForPath(u.Path)
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "GET"
	}
	return ControlCall{
		Method:  method,
		URL:     u,
		Path:    u.RequestURI(),
		Verdict: controlVerdictWithMethod(u, method),
	}, true
}

func controlVerdictForToolUse(t conversation.ToolUse) (inspector.Verdict, bool) {
	call, ok := ParseControlToolUse(t)
	if ok {
		return call.Verdict, true
	}
	return inspector.Verdict{}, false
}

func controlCallParts(t conversation.ToolUse) (*url.URL, string, []byte, bool) {
	if len(t.Input) == 0 {
		return nil, "", nil, false
	}
	if u, method, body, ok := controlPartsFromStructuredInput(t.Input); ok {
		return u, method, body, true
	}
	if u, method, body, ok := controlPartsFromCommandInput(t.Input); ok {
		return u, method, body, true
	}
	return nil, "", nil, false
}

func controlURLFromStructuredInput(in json.RawMessage) (*url.URL, bool) {
	u, _, _, ok := controlPartsFromStructuredInput(in)
	return u, ok
}

func controlPartsFromStructuredInput(in json.RawMessage) (*url.URL, string, []byte, bool) {
	var raw struct {
		URL    string          `json:"url"`
		Method string          `json:"method,omitempty"`
		Body   json.RawMessage `json:"body,omitempty"`
	}
	if err := json.Unmarshal(in, &raw); err != nil || raw.URL == "" {
		return nil, "", nil, false
	}
	u, ok := parseControlURL(raw.URL)
	if !ok {
		return nil, "", nil, false
	}
	body := raw.Body
	var bodyString string
	if len(body) > 0 && json.Unmarshal(body, &bodyString) == nil {
		body = []byte(bodyString)
	}
	return u, raw.Method, body, true
}

func controlURLFromCommandInput(in json.RawMessage) (*url.URL, bool) {
	u, _, _, ok := controlPartsFromCommandInput(in)
	return u, ok
}

func controlPartsFromCommandInput(in json.RawMessage) (*url.URL, string, []byte, bool) {
	var raw struct {
		Cmd     string `json:"cmd,omitempty"`
		Command string `json:"command,omitempty"`
	}
	if err := json.Unmarshal(in, &raw); err != nil {
		return nil, "", nil, false
	}
	cmd := strings.TrimSpace(raw.Cmd)
	if cmd == "" {
		cmd = strings.TrimSpace(raw.Command)
	}
	if cmd == "" || !strings.HasPrefix(strings.ToLower(cmd), "curl ") {
		return nil, "", nil, false
	}
	if hasControlRewriteUnsafeShell(cmd) {
		return nil, "", nil, false
	}
	tokens, ok := controlShellTokenize(cmd)
	if !ok || len(tokens) == 0 {
		return nil, "", nil, false
	}
	method := ""
	var body []byte
	var control *url.URL
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		case tok == "-X" || tok == "--request":
			if i+1 >= len(tokens) {
				return nil, "", nil, false
			}
			method = tokens[i+1]
			i++
		case tok == "-d" || tok == "--data" || tok == "--data-raw" || tok == "--data-binary":
			if i+1 >= len(tokens) {
				return nil, "", nil, false
			}
			body = []byte(tokens[i+1])
			i++
		case strings.HasPrefix(tok, "--data="):
			body = []byte(strings.TrimPrefix(tok, "--data="))
		case strings.HasPrefix(tok, "--data-raw="):
			body = []byte(strings.TrimPrefix(tok, "--data-raw="))
		case strings.HasPrefix(tok, "--data-binary="):
			body = []byte(strings.TrimPrefix(tok, "--data-binary="))
		default:
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				if u, ok := parseControlURL(tok); ok {
					control = u
				}
			}
		}
	}
	if control == nil {
		return nil, "", nil, false
	}
	if method == "" && len(body) > 0 {
		method = "POST"
	}
	return control, method, body, true
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
	return controlVerdictWithMethod(u, controlMethodForPath(u.Path))
}

func controlVerdictWithMethod(u *url.URL, method string) inspector.Verdict {
	return inspector.Verdict{
		IsAPICall: true,
		Method:    method,
		Host:      u.Hostname(),
		Path:      u.RequestURI(),
		Source:    inspector.SourceDeterministic,
		Reason:    "synthetic Clawvisor control endpoint",
	}
}

func controlMethodForPath(path string) string {
	if strings.HasSuffix(path, "/tasks") {
		return "POST"
	}
	return "GET"
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

func controlShellTokenize(cmd string) ([]string, bool) {
	var (
		tokens []string
		buf    strings.Builder
		state  rune
	)
	flush := func() {
		if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
	}
	for i := range len(cmd) {
		c := rune(cmd[i])
		switch {
		case state == 0 && (c == ' ' || c == '\t' || c == '\n'):
			flush()
		case state == 0 && (c == '\'' || c == '"'):
			state = c
		case state != 0 && c == state:
			state = 0
		default:
			buf.WriteRune(c)
		}
	}
	if state != 0 {
		return nil, false
	}
	flush()
	return tokens, true
}
