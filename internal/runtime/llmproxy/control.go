package llmproxy

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
	"mvdan.cc/sh/v3/syntax"
)

const (
	ControlSyntheticHost  = "clawvisor.local"
	ControlNoticeSentinel = "Clawvisor proxy-lite control plane."
)

func ControlNotice(controlBaseURL string, availableTools []string) string {
	return controlNotice(controlBaseURL, availableTools, nil)
}

func ControlNoticeWithPolicy(controlBaseURL string, availableTools []string, toolRules []*store.RuntimePolicyRule) string {
	return controlNotice(controlBaseURL, availableTools, toolRules)
}

func controlNotice(controlBaseURL string, availableTools []string, toolRules []*store.RuntimePolicyRule) string {
	// Always advertise the synthetic URL. Clawvisor rewrites it to the
	// real daemon URL transparently and mints fresh auth on every call.
	// Models that see (or guess) the daemon URL and call it directly
	// bypass the rewrite path and end up reusing one-shot nonces from
	// prior turns. controlBaseURL is intentionally ignored here.
	_ = controlBaseURL
	docsURL := "https://" + ControlSyntheticHost + "/control/skill"
	vaultItemsURL := "https://" + ControlSyntheticHost + "/control/vault/items"
	tasksURL := "https://" + ControlSyntheticHost + "/control/tasks"
	tasksURLInline := tasksURL + "?surface=inline"
	toolExamples := controlToolExamples(availableTools)
	shellTool := controlShellTool(availableTools)
	if shellTool == "" {
		shellTool = "bash"
	}
	allowedLines := controlAllowedWithoutTaskLines(availableTools, toolRules)
	workedExampleLines := controlWorkedExampleLines(tasksURLInline, shellTool, availableTools)
	return strings.Join([]string{
		ControlNoticeSentinel,
		"",
		"WORKFLOW — start every non-trivial request with a task.",
		"",
		"Create a task before multi-step work, writing files, making network calls, changing state, or using credentials. Don't wait for a tool call to be refused before creating a task.",
		"",
		"Task endpoint:",
		"  - Interactive user present: POST " + tasksURLInline,
		"  - Headless/background run: POST " + tasksURL,
		"",
		"Required task shape:",
		"  {\"purpose\":\"<user-visible goal>\",",
		"   \"expected_tools_json\":[{\"tool_name\":\"" + shellTool + "\",\"why\":\"<why this tool is needed>\"}],",
		"   \"intent_verification_mode\":\"strict\",",
		"   \"expires_in_seconds\":600}",
		"",
		"If credentials are needed, add:",
		"  \"required_credentials_json\":[{\"vault_item_id\":\"<vault item id>\",\"why\":\"<why this credential is needed>\"}]",
		"",
		strings.Join(workedExampleLines, "\n"),
		"",
		"Field rules:",
		"  - `expected_tools_json`: use actual available tools (" + toolExamples + "). List plausible tools up front; include verify/read commands in the same tool `why`.",
		"  - `required_credentials_json`: OMIT unless credentials are needed. If included, every entry MUST include `vault_item_id` or `vault_item_handle` AND `why`.",
		"  - Invalid credential request: `\"required_credentials_json\":[{\"vault_item_id\":\"github\"}]`",
		"",
		strings.Join(allowedLines, "\n"),
		"",
		"CREDENTIAL ACCESS:",
		"  - If you already have an `autovault_...` placeholder, do NOT call " + vaultItemsURL + " just to identify it. Create the task for the intended API call, omit `required_credentials_json`, and use the placeholder directly after approval.",
		"  - Use GET " + vaultItemsURL + " only when you need Clawvisor to mint a new placeholder from an available vault item.",
		"  - If you need a new placeholder, declare the selected vault item in `required_credentials_json` with a concrete `why`.",
		"  - Do not ask the user to paste raw secrets into chat.",
		"",
		"VAULT PLACEHOLDERS — values like `autovault_github_xyz` are NOT raw credentials and are already usable placeholders. Use them directly in headers or curl arguments after the task is approved; Clawvisor substitutes the real secret at proxy time. Raw tokens such as `ghp_...` or `sk-...` are sensitive; ask the user to vault them first.",
		"",
		"Control-plane rules:",
		"  - Before creating the task, tell me I will need to approve it.",
		"  - Task creation does not grant permission until I approve it.",
		"  - Use `" + shellTool + "` with curl for control-plane calls.",
		"  - Use one foreground curl with JSON via `--data @-`; no temp files, pipes, redirects, extra shell commands, `&`, `nohup`, or polling.",
		"  - NEVER write `cv-nonce-...`, `X-Clawvisor-Caller`, `X-Clawvisor-Target-Host`, or any `X-Clawvisor-*` header. Clawvisor injects those.",
		"  - NEVER call `http://localhost:<port>` or `http://127.0.0.1:<port>` directly. Use `https://" + ControlSyntheticHost + "`.",
		"  - Do NOT prefix tool calls with `CLAWVISOR_TASK_ID=<id>`.",
		"For schemas and examples, GET " + docsURL + ".",
		"",
		"Canonical task curl:",
		"  curl -sS -X POST '" + tasksURLInline + "' \\",
		"    -H 'Content-Type: application/json' \\",
		"    --data @- <<'JSON'",
		"  {\"purpose\":\"<user-visible goal>\",",
		"   \"expected_tools_json\":[{\"tool_name\":\"" + shellTool + "\",\"why\":\"<why this tool is needed>\"}],",
		"   \"intent_verification_mode\":\"strict\",",
		"   \"expires_in_seconds\":600}",
		"  JSON",
	}, "\n")
}

func controlWorkedExampleLines(tasksURLInline, shellTool string, availableTools []string) []string {
	readTool := controlToolByName(availableTools, "read")
	writeTool := controlToolByName(availableTools, "write")
	localTools := []string{
		"{\"tool_name\":\"" + shellTool + "\",\"why\":\"Create the target directory and run sanity checks such as ls and wc after files are written.\"}",
	}
	if writeTool != "" {
		localTools = append(localTools, "{\"tool_name\":\""+writeTool+"\",\"why\":\"Write each fake conversation file into the target directory.\"}")
	}
	if readTool != "" {
		localTools = append(localTools, "{\"tool_name\":\""+readTool+"\",\"why\":\"Read back the written files to verify their contents.\"}")
	}
	githubTools := []string{}
	if readTool != "" {
		githubTools = append(githubTools, "{\"tool_name\":\""+readTool+"\",\"why\":\"Read local deployment check logs to summarize the failure.\"}")
	}
	githubTools = append(githubTools, "{\"tool_name\":\""+shellTool+"\",\"why\":\"Call the GitHub API with curl to create the requested issue.\"}")
	return []string{
		"Worked example — multi-step local files, no credentials:",
		"  curl -sS -X POST '" + tasksURLInline + "' \\",
		"    -H 'Content-Type: application/json' \\",
		"    --data @- <<'JSON'",
		"  {\"purpose\":\"Create a temporary conversation fixture directory and verify the written files\",",
		"   \"expected_tools_json\":[" + strings.Join(localTools, ",") + "],",
		"   \"intent_verification_mode\":\"strict\",",
		"   \"expires_in_seconds\":600}",
		"  JSON",
		"",
		"Worked example — credentialed GitHub task:",
		"  curl -sS -X POST '" + tasksURLInline + "' \\",
		"    -H 'Content-Type: application/json' \\",
		"    --data @- <<'JSON'",
		"  {\"purpose\":\"Create a GitHub issue summarizing the failing deployment check\",",
		"   \"expected_tools_json\":[" + strings.Join(githubTools, ",") + "],",
		"   \"required_credentials_json\":[{\"vault_item_id\":\"github\",\"why\":\"Authenticate to GitHub to create the approved issue.\"}],",
		"   \"intent_verification_mode\":\"strict\",",
		"   \"expires_in_seconds\":600}",
		"  JSON",
	}
}

func controlAllowedWithoutTaskLines(availableTools []string, toolRules []*store.RuntimePolicyRule) []string {
	tools := compactToolNames(availableTools)
	policyAllowed := policyAllowedToolNames(tools, toolRules)
	lines := []string{
		"ALLOWED WITHOUT A TASK — for single-step, non-destructive inspection:",
	}
	if len(policyAllowed) > 0 {
		lines = append(lines, "  - Active policy allowlists "+formatToolList(policyAllowed)+".")
	}
	if len(lines) == 1 {
		lines = append(lines, "  - None yet. Use the dashboard Tool Controls to always allow specific tools.")
	}
	return lines
}

func policyAllowedToolNames(availableTools []string, toolRules []*store.RuntimePolicyRule) []string {
	if len(availableTools) == 0 || len(toolRules) == 0 {
		return nil
	}
	byLower := map[string]string{}
	for _, tool := range compactToolNames(availableTools) {
		byLower[strings.ToLower(tool)] = tool
	}
	seen := map[string]struct{}{}
	var out []string
	for _, rule := range toolRules {
		if rule == nil || !rule.Enabled || rule.Kind != "tool" || rule.Action != "allow" {
			continue
		}
		if name := strings.TrimSpace(rule.ToolName); name != "" {
			if actual, ok := byLower[strings.ToLower(name)]; ok {
				if _, exists := seen[strings.ToLower(actual)]; !exists {
					seen[strings.ToLower(actual)] = struct{}{}
					out = append(out, actual)
				}
			}
		}
	}
	return out
}

func formatToolList(tools []string) string {
	quoted := make([]string, 0, len(tools))
	for _, tool := range tools {
		quoted = append(quoted, "`"+tool+"`")
	}
	return strings.Join(quoted, " / ")
}

func controlToolExamples(availableTools []string) string {
	tools := compactToolNames(availableTools)
	if len(tools) == 0 {
		return "Bash, Read, Write, Edit, WebFetch, etc."
	}
	const max = 8
	if len(tools) > max {
		tools = tools[:max]
		return strings.Join(tools, ", ") + ", etc."
	}
	return strings.Join(tools, ", ")
}

func controlShellTool(availableTools []string) string {
	for _, tool := range compactToolNames(availableTools) {
		switch strings.ToLower(tool) {
		case "bash", "shell", "exec", "exec_command":
			return tool
		}
	}
	return ""
}

func controlToolByName(availableTools []string, name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, tool := range compactToolNames(availableTools) {
		if strings.ToLower(tool) == name {
			return tool
		}
	}
	return ""
}

func compactToolNames(availableTools []string) []string {
	out := make([]string, 0, len(availableTools))
	seen := make(map[string]struct{}, len(availableTools))
	for _, tool := range availableTools {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			continue
		}
		key := strings.ToLower(tool)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, tool)
	}
	return out
}

// InjectControlNotice adds a compact control-plane hint to the request context.
// The synthetic URL is rewritten from model-emitted tool calls before the tool
// runner sees it, so the prompt stays stable across local and public daemon URLs.
func InjectControlNotice(provider conversation.Provider, body []byte, controlBaseURL string, availableTools []string) ([]byte, bool, error) {
	return InjectControlNoticeWithPolicy(provider, body, controlBaseURL, availableTools, nil)
}

func InjectControlNoticeWithPolicy(provider conversation.Provider, body []byte, controlBaseURL string, availableTools []string, toolRules []*store.RuntimePolicyRule) ([]byte, bool, error) {
	if controlNoticeAlreadyPresent(provider, body) {
		return body, false, nil
	}
	notice := ControlNoticeWithPolicy(controlBaseURL, availableTools, toolRules)
	switch provider {
	case conversation.ProviderAnthropic:
		return injectAnthropicControlNotice(body, notice)
	case conversation.ProviderOpenAI:
		return injectOpenAIControlNotice(body, notice)
	default:
		return body, false, nil
	}
}

func controlNoticeAlreadyPresent(provider conversation.Provider, body []byte) bool {
	switch provider {
	case conversation.ProviderAnthropic:
		return anthropicSystemContains(body, ControlNoticeSentinel)
	case conversation.ProviderOpenAI:
		return openAISystemContains(body, ControlNoticeSentinel)
	default:
		return false
	}
}

func anthropicSystemContains(body []byte, needle string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	return rawSystemContains(raw["system"], needle)
}

func openAISystemContains(body []byte, needle string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	if rawSystemContains(raw["instructions"], needle) {
		return true
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw["messages"], &messages); err != nil || len(messages) == 0 {
		return false
	}
	for _, msg := range messages {
		var role string
		if err := json.Unmarshal(msg["role"], &role); err != nil {
			continue
		}
		if role != "system" && role != "developer" {
			return false
		}
		if rawSystemContains(msg["content"], needle) {
			return true
		}
	}
	return false
}

func rawSystemContains(raw json.RawMessage, needle string) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.Contains(s, needle)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, block := range blocks {
			if text, _ := block["text"].(string); strings.Contains(text, needle) {
				return true
			}
		}
	}
	return strings.Contains(string(raw), needle)
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
	v, ok := controlVerdictForToolUse(t, controlBaseURL)
	if !ok {
		return nil, inspector.Verdict{}, false, nil
	}
	opts := inspector.DefaultRewriteOpts(controlBaseURL)
	opts.CallerToken = callerToken
	if rewritten, ok, err := rewriteControlCommandToolUse(t, v, opts); ok {
		return rewritten, v, true, err
	}
	rewritten, err := inspector.Rewrite(inspector.ToolUse{
		ID:    t.ID,
		Name:  t.Name,
		Input: t.Input,
	}, v, opts)
	return rewritten, v, true, err
}

func rewriteControlCommandToolUse(t conversation.ToolUse, v inspector.Verdict, opts inspector.RewriteOpts) ([]byte, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(t.Input, &raw); err != nil {
		return nil, false, nil
	}
	cmdField := "cmd"
	cmd, ok := raw["cmd"].(string)
	if !ok {
		cmdField = "command"
		cmd, ok = raw["command"].(string)
	}
	if !ok || cmd == "" {
		return nil, false, nil
	}
	rewritten, ok := rewriteControlCommandString(cmd, v, opts)
	if !ok {
		return nil, false, nil
	}
	raw[cmdField] = rewritten
	// Codex's exec_command backgrounds the call when yield_time_ms
	// elapses. The default tends to be ~1s, which is too short for
	// user-mediated control calls — without clamping, the agent's task
	// POST gets backgrounded and the agent proceeds before the user can
	// approve. Mention of yield_time_ms in the prompt only makes the
	// model cargo-cult a small value back, so clamp here. Harmless on
	// Bash (Claude Code has no such parameter).
	clampControlToolUseTimeouts(raw, t.Name)
	out, err := json.Marshal(raw)
	return out, true, err
}

func RewriteControlFailureToolUse(t conversation.ToolUse, controlBaseURL string, callerToken string, reason string) ([]byte, bool, error) {
	if strings.TrimSpace(controlBaseURL) == "" {
		return nil, false, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(t.Input, &raw); err != nil {
		return nil, false, nil
	}
	cmdField := "cmd"
	original, ok := raw["cmd"].(string)
	if !ok {
		cmdField = "command"
		original, ok = raw["command"].(string)
		if !ok {
			return nil, false, nil
		}
	}
	u, err := url.Parse(strings.TrimRight(controlBaseURL, "/") + "/control/failure")
	if err != nil {
		return nil, false, err
	}
	q := u.Query()
	if strings.TrimSpace(reason) == "" {
		reason = "malformed_control_command"
	}
	q.Set("reason", reason)
	u.RawQuery = q.Encode()
	body, err := json.Marshal(map[string]string{
		"original_tool":    t.Name,
		"original_command": sanitizeControlFailureCommand(original),
	})
	if err != nil {
		return nil, false, err
	}
	raw[cmdField] = strings.Join([]string{
		"curl",
		"-sS",
		"-X", "POST",
		"-H", shellQuote("Content-Type: application/json"),
		"-H", shellQuote("X-Clawvisor-Target-Host: " + ControlSyntheticHost),
		"-H", shellQuote("X-Clawvisor-Caller: Bearer " + callerToken),
		"--data", shellQuote(string(body)),
		shellQuote(u.String()),
	}, " ")
	clampControlToolUseTimeouts(raw, t.Name)
	out, err := json.Marshal(raw)
	return out, true, err
}

func sanitizeControlFailureCommand(cmd string) string {
	cmd = regexp.MustCompile(`cv-nonce-[A-Za-z0-9_-]+`).ReplaceAllString(cmd, "cv-nonce-REDACTED")
	cmd = regexp.MustCompile(`(?i)(X-Clawvisor-Caller:\s*Bearer\s+)[^'"\s]+`).ReplaceAllString(cmd, "${1}REDACTED")
	authBearer := regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[^'"\s]+`)
	cmd = authBearer.ReplaceAllStringFunc(cmd, func(match string) string {
		parts := authBearer.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		token := strings.TrimPrefix(match, parts[1])
		if strings.HasPrefix(strings.ToLower(token), "autovault_") {
			return match
		}
		return parts[1] + "REDACTED"
	})
	return cmd
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// controlToolUseMinYieldMs is the floor we clamp Codex's
// exec_command yield_time_ms to when the call is a /control/* curl.
// The curl's max block is wait timeout (120s) plus network slop; 180s
// gives a comfortable margin without forcing the agent to wait
// substantially longer than necessary if the user replies quickly.
const controlToolUseMinYieldMs = 180_000

func clampControlToolUseTimeouts(raw map[string]any, toolName string) {
	if raw == nil {
		return
	}
	// Always clamp an EXISTING small yield_time_ms regardless of tool
	// name — the field has a single meaning across the harnesses that
	// adopt it (Codex's exec_command today), and a stale small value
	// still backgrounds the control curl.
	if cur, ok := numericFromAny(raw["yield_time_ms"]); ok {
		if cur < controlToolUseMinYieldMs {
			raw["yield_time_ms"] = controlToolUseMinYieldMs
		}
		return
	}
	// INTRODUCING a yield_time_ms field is a Codex-specific repair:
	// the field doesn't exist on Bash or any other harness's tool
	// shape, and stamping it onto a future cmd-keyed tool that
	// doesn't use yield_time_ms as its yield parameter would be a
	// stray field at best, a silent shape-corruption at worst. Gate
	// strictly by tool name.
	if toolName != "exec_command" {
		return
	}
	if _, hasCmd := raw["cmd"]; !hasCmd {
		return
	}
	// `cmd` field present + no yield_time_ms = Codex exec_command
	// using the harness default (~1s). Set the field explicitly so
	// the harness keeps the curl in the foreground long enough.
	raw["yield_time_ms"] = controlToolUseMinYieldMs
}

// numericFromAny coerces an interface{} from a json.Unmarshal-decoded
// map (always float64 for JSON numbers) into int64. Returns (0, false)
// when the value isn't a number.
func numericFromAny(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func rewriteControlCommandString(cmd string, v inspector.Verdict, opts inspector.RewriteOpts) (string, bool) {
	resolver, err := url.Parse(opts.ResolverBaseURL)
	if err != nil || resolver.Host == "" {
		return "", false
	}
	args, ok := parseControlCurlArgs(cmd)
	if !ok {
		return "", false
	}
	for _, arg := range args[1:] {
		rawURL := arg.value
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Hostname(), v.Host) {
			continue
		}
		rewritten := *parsed
		rewritten.Scheme = resolver.Scheme
		rewritten.Host = resolver.Host
		if resolver.Path != "" {
			rewritten.Path = strings.TrimRight(resolver.Path, "/") + parsed.Path
		}
		headers := " -H " + shellSingleQuote(firstNonEmptyControl(opts.TargetHostHeader, "X-Clawvisor-Target-Host")+": "+parsed.Host)
		if opts.CallerToken != "" && opts.CallerHeader != "" {
			headers += " -H " + shellSingleQuote(opts.CallerHeader+": Bearer "+opts.CallerToken)
		}
		return cmd[:arg.start] + headers + " " + shellSingleQuote(rewritten.String()) + cmd[arg.end:], true
	}
	return "", false
}

func firstNonEmptyControl(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func shellSingleQuote(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type ControlCall struct {
	Method  string
	URL     *url.URL
	Path    string
	Body    []byte
	Verdict inspector.Verdict
}

func ParseControlToolUse(t conversation.ToolUse) (ControlCall, bool) {
	return ParseControlToolUseWithBase(t, "")
}

func ParseControlToolUseWithBase(t conversation.ToolUse, controlBaseURL string) (ControlCall, bool) {
	u, method, _, ok := controlCallParts(t, controlBaseURL)
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

func controlVerdictForToolUse(t conversation.ToolUse, controlBaseURL string) (inspector.Verdict, bool) {
	call, ok := ParseControlToolUseWithBase(t, controlBaseURL)
	if ok {
		return call.Verdict, true
	}
	return inspector.Verdict{}, false
}

func controlCallParts(t conversation.ToolUse, controlBaseURL string) (*url.URL, string, []byte, bool) {
	if len(t.Input) == 0 {
		return nil, "", nil, false
	}
	if u, method, body, ok := controlPartsFromStructuredInput(t.Input, controlBaseURL); ok {
		return u, method, body, true
	}
	if u, method, body, ok := controlPartsFromCommandInput(t.Input, controlBaseURL); ok {
		return u, method, body, true
	}
	return nil, "", nil, false
}

func controlToolUseMentionsEndpoint(t conversation.ToolUse, controlBaseURL string) bool {
	if len(t.Input) == 0 {
		return false
	}
	if u, ok := controlURLFromStructuredInput(t.Input); ok && isControlHost(u, controlBaseURL) {
		return true
	}
	return commandInputMentionsControlEndpoint(t.Input, controlBaseURL)
}

func controlURLFromStructuredInput(in json.RawMessage) (*url.URL, bool) {
	u, _, _, ok := controlPartsFromStructuredInput(in, "")
	return u, ok
}

func controlPartsFromStructuredInput(in json.RawMessage, controlBaseURL string) (*url.URL, string, []byte, bool) {
	var raw struct {
		URL    string          `json:"url"`
		Method string          `json:"method,omitempty"`
		Body   json.RawMessage `json:"body,omitempty"`
	}
	if err := json.Unmarshal(in, &raw); err != nil || raw.URL == "" {
		return nil, "", nil, false
	}
	u, ok := parseControlURL(raw.URL, controlBaseURL)
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
	u, _, _, ok := controlPartsFromCommandInput(in, "")
	return u, ok
}

func commandInputMentionsControlEndpoint(in json.RawMessage, controlBaseURL string) bool {
	var raw struct {
		Cmd     string `json:"cmd,omitempty"`
		Command string `json:"command,omitempty"`
	}
	if err := json.Unmarshal(in, &raw); err != nil {
		return false
	}
	cmd := raw.Cmd
	if strings.TrimSpace(cmd) == "" {
		cmd = raw.Command
	}
	return textMentionsControlEndpoint(cmd, controlBaseURL)
}

func textMentionsControlEndpoint(text string, controlBaseURL string) bool {
	if strings.Contains(text, "://"+ControlSyntheticHost+"/control") {
		return true
	}
	base, err := url.Parse(strings.TrimSpace(controlBaseURL))
	if err != nil || base.Host == "" {
		return false
	}
	prefix := strings.TrimRight(controlBaseURL, "/") + "/control"
	if strings.Contains(text, prefix) {
		return true
	}
	if base.Scheme != "" && strings.Contains(text, base.Scheme+"://"+base.Host+"/control") {
		return true
	}
	return false
}

func controlPartsFromCommandInput(in json.RawMessage, controlBaseURL string) (*url.URL, string, []byte, bool) {
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
	if cmd == "" {
		return nil, "", nil, false
	}
	args, dataFiles, ok := parseControlCmd(cmd)
	if !ok {
		return nil, "", nil, false
	}
	u, method, body, ok := controlPartsFromCurlArgs(args, controlBaseURL)
	if !ok {
		return nil, "", nil, false
	}
	// curl --data @path resolves to the prior cat-heredoc body so the
	// inline intercept can read the model's task definition without
	// the curl actually running.
	if len(dataFiles) > 0 && len(body) > 0 && body[0] == '@' {
		path := string(body[1:])
		if resolved, ok := dataFiles[path]; ok {
			body = resolved
		}
	}
	return u, method, body, true
}

func controlPartsFromCurlArgs(args []controlCurlArg, controlBaseURL string) (*url.URL, string, []byte, bool) {
	method := ""
	var body []byte
	var control *url.URL
	for i := 1; i < len(args); i++ {
		tok := args[i].value
		switch {
		case tok == "-X" || tok == "--request":
			if i+1 >= len(args) {
				return nil, "", nil, false
			}
			method = args[i+1].value
			i++
		case strings.HasPrefix(tok, "-X") && tok != "-X":
			method = strings.TrimPrefix(tok, "-X")
		case strings.HasPrefix(tok, "--request="):
			method = strings.TrimPrefix(tok, "--request=")
		case tok == "-d" || tok == "--data" || tok == "--data-raw" || tok == "--data-binary":
			if i+1 >= len(args) {
				return nil, "", nil, false
			}
			body = []byte(args[i+1].value)
			i++
		case strings.HasPrefix(tok, "-d") && tok != "-d":
			body = []byte(strings.TrimPrefix(tok, "-d"))
		case strings.HasPrefix(tok, "--data="):
			body = []byte(strings.TrimPrefix(tok, "--data="))
		case strings.HasPrefix(tok, "--data-raw="):
			body = []byte(strings.TrimPrefix(tok, "--data-raw="))
		case strings.HasPrefix(tok, "--data-binary="):
			body = []byte(strings.TrimPrefix(tok, "--data-binary="))
		default:
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				u, ok := parseControlURL(tok, controlBaseURL)
				if !ok {
					// A non-control URL alongside a control URL would
					// let a curl invocation claim policy-bypass status
					// for the control call while still hitting an
					// arbitrary outbound URL. Refuse the entire command
					// rather than rewriting only the matching URL.
					return nil, "", nil, false
				}
				if control != nil {
					// Multiple control URLs in one invocation is
					// ambiguous; refuse instead of guessing.
					return nil, "", nil, false
				}
				control = u
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

func parseControlURL(raw string, controlBaseURL string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false
	}
	if !isControlHost(u, controlBaseURL) {
		return nil, false
	}
	if u.Path != "/control" && !strings.HasPrefix(u.Path, "/control/") {
		return nil, false
	}
	return u, true
}

func isControlHost(u *url.URL, controlBaseURL string) bool {
	if strings.EqualFold(u.Hostname(), ControlSyntheticHost) {
		return true
	}
	base, err := url.Parse(strings.TrimSpace(controlBaseURL))
	if err != nil || base.Host == "" {
		return false
	}
	return strings.EqualFold(u.Hostname(), base.Hostname()) && samePort(u, base)
}

func samePort(a, b *url.URL) bool {
	ap := a.Port()
	if ap == "" {
		ap = defaultPort(a.Scheme)
	}
	bp := b.Port()
	if bp == "" {
		bp = defaultPort(b.Scheme)
	}
	return ap == bp
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
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

type controlCurlArg struct {
	value string
	start int
	end   int
}

func parseControlCurlArgs(cmd string) ([]controlCurlArg, bool) {
	args, _, ok := parseControlCmd(cmd)
	return args, ok
}

// parseControlCmd accepts either a single curl statement or a
// multi-statement script of the form
//
//	cat <<TAG >$staticpath     # (zero or more such writes)
//	$body
//	TAG
//	curl ... --data @$staticpath ...
//
// and returns (a) the curl statement's args with their absolute offsets
// in the original cmd string, and (b) a map of paths the prior cat
// statements wrote, so a curl `--data @path` can be resolved to the
// inline body. The curl's own stdin heredoc is also registered under
// the special key "-" so `--data @-` resolves to its body. Any shape
// outside this allowlist (extra commands, pipes, subshells, variable
// expansion in paths, …) refuses closed.
func parseControlCmd(cmd string) ([]controlCurlArg, map[string][]byte, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil || len(file.Stmts) == 0 {
		return nil, nil, false
	}
	var curlStmt *syntax.Stmt
	// The cat-heredoc form exists solely to materialize the curl's
	// `--data @path` body. We allow at most one cat statement and it
	// must come strictly before the curl. After parsing, the cat's
	// path must match the curl's `--data @path` target — otherwise
	// it's a smuggled file write to an unrelated location that would
	// survive into the rewritten command (the rewriter only edits the
	// curl URL; surrounding statements pass through verbatim).
	var catPath string
	var catBody []byte
	seenCat := false
	for i, stmt := range file.Stmts {
		// A trailing `;` is fine; non-trailing `;` or `&` between
		// commands smuggles in extra side effects we can't reason
		// about safely, so refuse.
		if stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
			return nil, nil, false
		}
		if stmt.Semicolon.IsValid() && i != len(file.Stmts)-1 {
			return nil, nil, false
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return nil, nil, false
		}
		head, ok := staticShellWord(call.Args[0])
		if !ok {
			return nil, nil, false
		}
		switch head {
		case "curl":
			if curlStmt != nil {
				return nil, nil, false
			}
			curlStmt = stmt
		case "cat":
			if seenCat || curlStmt != nil {
				// Multiple cats or a cat after the curl could write
				// arbitrary additional files that the curl never reads.
				return nil, nil, false
			}
			path, body, ok := parseHeredocToFile(stmt, call)
			if !ok {
				return nil, nil, false
			}
			catPath = path
			catBody = body
			seenCat = true
		default:
			return nil, nil, false
		}
	}
	if curlStmt == nil {
		return nil, nil, false
	}
	args, ok := parseSingleControlCurlStmt(cmd, curlStmt)
	if !ok {
		return nil, nil, false
	}
	dataFiles := map[string][]byte{}
	if seenCat {
		// The cat must be the curl's body source; if curl doesn't
		// read @catPath we'd be allowing an unused file write.
		if !curlReadsDataAtPath(args, catPath) {
			return nil, nil, false
		}
		dataFiles[catPath] = catBody
	}
	// Capture the curl's own stdin heredoc so `--data @-` resolves to
	// its body. This is the canonical shape the proxy's prompt teaches
	// the model:
	//
	//	curl ... --data @- <<'JSON'
	//	{...}
	//	JSON
	if body, ok := stdinHeredocBody(curlStmt); ok {
		dataFiles["-"] = body
	}
	if len(dataFiles) == 0 {
		dataFiles = nil
	}
	return args, dataFiles, true
}

// curlReadsDataAtPath returns true when the curl args contain a
// `--data @<path>` (or -d, --data-raw, --data-binary in any of their
// `=` / split forms) whose target is exactly the given path. Used to
// confirm a cat-heredoc statement is the curl's body source rather
// than a smuggled write to an unrelated location.
func curlReadsDataAtPath(args []controlCurlArg, path string) bool {
	if path == "" {
		return false
	}
	target := "@" + path
	for i := 1; i < len(args); i++ {
		tok := args[i].value
		switch {
		case tok == "-d" || tok == "--data" || tok == "--data-raw" || tok == "--data-binary":
			if i+1 < len(args) && args[i+1].value == target {
				return true
			}
		case strings.HasPrefix(tok, "-d") && tok != "-d":
			if strings.TrimPrefix(tok, "-d") == target {
				return true
			}
		case strings.HasPrefix(tok, "--data="):
			if strings.TrimPrefix(tok, "--data=") == target {
				return true
			}
		case strings.HasPrefix(tok, "--data-raw="):
			if strings.TrimPrefix(tok, "--data-raw=") == target {
				return true
			}
		case strings.HasPrefix(tok, "--data-binary="):
			if strings.TrimPrefix(tok, "--data-binary=") == target {
				return true
			}
		}
	}
	return false
}

// stdinHeredocBody returns the heredoc body redirected into stdin for
// the given statement, if any. Used so a curl `--data @-` invocation
// can pick up the body the model wrote between <<TAG and TAG.
func stdinHeredocBody(stmt *syntax.Stmt) ([]byte, bool) {
	if stmt == nil {
		return nil, false
	}
	for _, redir := range stmt.Redirs {
		if redir.Op != syntax.Hdoc && redir.Op != syntax.DashHdoc {
			continue
		}
		if redir.Hdoc == nil {
			continue
		}
		body, ok := staticShellWord(redir.Hdoc)
		if !ok {
			continue
		}
		return []byte(body), true
	}
	return nil, false
}

// parseSingleControlCurlStmt extracts the curl args from a single shell
// statement, mirroring the strict single-stmt rules the parser used
// before multi-stmt support: no negate/background/coprocess/disown,
// allowed redirs are stdin heredocs to static words, no variable
// assignments, args must be statically expandable, and args[0] must
// be `curl`.
func parseSingleControlCurlStmt(cmd string, stmt *syntax.Stmt) ([]controlCurlArg, bool) {
	if stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return nil, false
	}
	for _, redir := range stmt.Redirs {
		if redir.Op != syntax.Hdoc && redir.Op != syntax.DashHdoc {
			return nil, false
		}
		if _, ok := staticShellWord(redir.Word); !ok {
			return nil, false
		}
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 {
		return nil, false
	}
	args := make([]controlCurlArg, 0, len(call.Args))
	for _, word := range call.Args {
		value, ok := staticShellWord(word)
		if !ok {
			return nil, false
		}
		start, end := int(word.Pos().Offset()), int(word.End().Offset())
		if start < 0 || end <= start || end > len(cmd) {
			return nil, false
		}
		args = append(args, controlCurlArg{value: value, start: start, end: end})
	}
	if args[0].value != "curl" {
		return nil, false
	}
	return args, true
}

// safeCatTargetPath restricts cat-heredoc output to a narrow temp-file
// shape so a model that uses the multi-statement form can't pick the
// path. Even though the parser already requires the cat's path to be
// the curl's `--data @path` target, the cat still executes on the
// harness — so without a path allowlist a model could write or
// overwrite arbitrary files (think `cat <<X >~/.bashrc`,
// `>/etc/important.conf`, `>/Users/<u>/.ssh/authorized_keys`) while
// the request looks like a normal /control/tasks call.
//
// The allowed shape is `/tmp/<flat-name>.json`:
//   - absolute, anchored at `/tmp/` — no $HOME expansion, no relative
//   - no subdirectories under `/tmp/` (the body file is flat)
//   - filename must START with an alnum or underscore — leading `.`
//     would create a dotfile (`/tmp/.bashrc.json`), leading `-` could
//     trip future tooling that walks `/tmp/` and parses filenames as
//     flags (`-rf.json` to a careless `find … -delete`). Neither is
//     security-critical given the `.json` suffix, but the goal of the
//     allowlist is "narrow and obviously safe," not "narrow with
//     surprising edges."
//   - filename body limited to safe chars; ends in `.json` so we
//     don't accidentally clobber a binary, dotfile, or shell init
//     script
//   - the parser separately requires the path to be statically
//     expandable, so `$HOME`/`$(…)`/`${…}` are already rejected
//     upstream.
//
// Filename body allows alnum/underscore/hyphen segments separated by
// single dots, ending with a literal `.json`. This rules out
// `/tmp/foo..bar.json`, `/tmp/...json`, etc. — paths that aren't
// security-critical given the suffix lock-in, but contradict the
// "narrow and obviously safe" property the comment articulates.
var safeCatTargetPath = regexp.MustCompile(`^/tmp/[A-Za-z0-9_][A-Za-z0-9_\-]*(\.[A-Za-z0-9_\-]+)*\.json$`)

// parseHeredocToFile recognizes
//
//	cat <<TAG >$staticpath
//	$body
//	TAG
//
// and returns ($staticpath, $body). Refuses any other cat-form
// (multiple redirs, pipes, dynamic path, append-mode `>>`, output path
// outside the safe temp-body allowlist).
func parseHeredocToFile(stmt *syntax.Stmt, call *syntax.CallExpr) (string, []byte, bool) {
	if len(call.Assigns) > 0 || len(call.Args) != 1 {
		return "", nil, false
	}
	// Exactly one heredoc and exactly one truncating `>` redirection.
	// More than one `>path` is a real smuggle: shell semantics for
	// `command >a >b` open AND truncate both files even though only
	// the last fd receives output, so `cat <<X >/private/file >/tmp/ok.json`
	// would silently zero out /private/file while the allowlist check
	// on the last path passes. Require exactly two redirs total
	// (1 Hdoc + 1 RdrOut) so we can't be tricked by extras.
	if len(stmt.Redirs) != 2 {
		return "", nil, false
	}
	var heredocBody string
	var outPath string
	hdocCount := 0
	outCount := 0
	for _, redir := range stmt.Redirs {
		switch redir.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			hdocCount++
			if redir.Hdoc == nil {
				return "", nil, false
			}
			body, ok := staticShellWord(redir.Hdoc)
			if !ok {
				return "", nil, false
			}
			heredocBody = body
		case syntax.RdrOut:
			// Truncating `>` only. Append `>>` is rejected — it
			// would let a model splice content onto an existing
			// file (history files, dotfiles, harness state) while
			// looking like a normal task-body write.
			outCount++
			path, ok := staticShellWord(redir.Word)
			if !ok || strings.TrimSpace(path) == "" {
				return "", nil, false
			}
			outPath = path
		default:
			return "", nil, false
		}
	}
	if hdocCount != 1 || outCount != 1 {
		return "", nil, false
	}
	if outPath == "" || heredocBody == "" {
		return "", nil, false
	}
	if !safeCatTargetPath.MatchString(outPath) {
		return "", nil, false
	}
	return outPath, []byte(heredocBody), true
}

func staticShellWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	return staticShellWordParts(word.Parts)
}

func staticShellWordParts(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			value, ok := staticShellWordParts(p.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(value)
		default:
			return "", false
		}
	}
	return b.String(), true
}
