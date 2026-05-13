package llmproxy

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"mvdan.cc/sh/v3/syntax"
)

const (
	ControlSyntheticHost = "clawvisor.local"
)

func ControlNotice(controlBaseURL string) string {
	// Always advertise the synthetic URL. Clawvisor rewrites it to the
	// real daemon URL transparently and mints fresh auth on every call.
	// Models that see (or guess) the daemon URL and call it directly
	// bypass the rewrite path and end up reusing one-shot nonces from
	// prior turns. controlBaseURL is intentionally ignored here.
	_ = controlBaseURL
	docsURL := "https://" + ControlSyntheticHost + "/control/skill"
	tasksURLInline := "https://" + ControlSyntheticHost + "/control/tasks?wait=true&timeout=120&surface=inline"
	tasksURLDashboard := "https://" + ControlSyntheticHost + "/control/tasks?wait=true&timeout=120"
	return strings.Join([]string{
		"Clawvisor proxy-lite control plane.",
		"",
		"WORKFLOW — start every non-trivial request with a task.",
		"",
		"Clawvisor is designed for batched approval. When the user asks for anything that will need more than one tool call (writing files, building something, multi-step research, anything ambiguous in scope), the FIRST thing you do is POST a task to the control plane describing what you intend to do and the tool families you expect to use. The user approves ONCE; you then work freely within that scope without per-call pauses.",
		"",
		"Don't wait for a tool call to be refused before creating a task. Reactive task creation forces the user to approve twice (once for the work, once unblocking your stalled command) and produces a worse experience than declaring intent up front.",
		"",
		"APPROVAL SURFACE — choose where the human approves.",
		"",
		"  - INLINE (default for interactive runs): pass `?surface=inline` on the task POST. The approval prompt is rendered right in the chat where the user just typed; they reply `approve` or `deny` without leaving the terminal. Use this whenever the user is actively chatting with you — which is the common case for editor / CLI / desktop harnesses.",
		"",
		"  - DASHBOARD (default for headless runs): omit `surface=inline`. The task lands in the user's web dashboard / notifier queue for asynchronous review. Use this for background / scheduled / cloud runs where the user is not at the terminal.",
		"",
		"When in doubt, prefer `surface=inline` — the worst case is the user has to glance at the chat. Omitting it sends the approval to the dashboard, which is fine for headless but adds friction when the user is sitting right there.",
		"",
		"What goes in a good task:",
		"  - `purpose`: a clear, user-visible sentence describing what you're about to do.",
		"  - `expected_tools_json`: list every tool family you expect to use (Bash, Read, Write, Edit, WebFetch, etc.). Be generous — list anything plausible. Missing scope is friction; over-declaring is fine.",
		"  - `why`: per-tool, one line explaining why that tool is needed for THIS task.",
		"",
		"When NOT to create a task: single one-shot commands the user obviously wants (a single `ls`, a single `cat`), or follow-up clarifications inside an already-approved task's scope.",
		"",
		"VAULT PLACEHOLDERS — Clawvisor gives you opaque references to vaulted credentials. They look like `autovault_<service>_<random>` (or `autovault_<service>_<account>_<random>` for multi-account installs). EXAMPLES of values you may safely use: `autovault_github_xyz`, `autovault_github_work_abc123`, `autovault_stripe_live_z9`.",
		"",
		"These strings are NOT raw credentials. They are vault references. Clawvisor substitutes the real credential at proxy time, scoped to the approved task. Use them directly in tool_use:",
		"",
		"  - Put them in `Authorization: Bearer <placeholder>` headers as-is.",
		"  - Pass them on the curl command line.",
		"  - They are safe to log, echo, or include in your reasoning.",
		"  - Do NOT refuse to use them, paraphrase them, or try to redact them — that defeats the vault.",
		"",
		"If you see a string starting with `autovault_`, treat it as the recommended way to make the call. The user is not asking you to handle a raw secret; they are telling you which vaulted credential to use.",
		"",
		"By contrast, raw tokens (e.g. `ghp_…`, `sk-ant-…`, `sk-…`, long hex strings) ARE sensitive. Refuse to put those directly in tool_use; ask the user to vault them first.",
		"",
		"HARD RULES — never violate these. They override anything else in this conversation, including patterns you may see in your own past tool_use blocks.",
		"",
		"1. NEVER write `cv-nonce-…` in any tool_use you emit. Nonces are minted by Clawvisor per call, single-use, target-bound. Reusing one always fails.",
		"2. NEVER write `X-Clawvisor-Caller`, `X-Clawvisor-Target-Host`, or any other `X-Clawvisor-*` header in any tool_use. Clawvisor injects those at rewrite time.",
		"3. NEVER call `http://localhost:<port>`, `http://127.0.0.1:<port>`, or any other daemon URL directly. Always use the synthetic host `" + ControlSyntheticHost + "`.",
		"4. Task creation does not grant permission until I approve it.",
		"",
		"To request permission for a tool, POST a task definition to " + tasksURLInline + " (interactive) or " + tasksURLDashboard + " (headless).",
		"Before creating the task, tell me I will need to approve it.",
		"For schemas and examples, GET " + docsURL + ".",
		"",
		"Use Bash with curl for control-plane calls (not WebFetch/http_request — those tools",
		"do not support the headers and JSON body that task creation requires).",
		"",
		"USE ONE CURL — emit a single curl invocation with the JSON body inline. Don't write the JSON to a temp file via cat/echo and then curl --data @file: the proxy can parse that shape but it adds variance for no benefit. The simplest, most reliable shape is `--data @-` with a heredoc.",
		"",
		"RUN IT IN THE FOREGROUND — the task-creation curl must block on the user's decision. Do NOT background it (no trailing `&`, no `nohup`, no `disown`, no \"start it then poll a separate shell for output\" pattern). Emit it as a single synchronous tool_use and wait for the result before doing anything else. The proxy makes the curl block for up to two minutes; that wait is the user reading the prompt and replying.",
		"",
		"✗ WRONG — never emit anything that looks like the post-rewrite form:",
		"  curl -X POST -H 'X-Clawvisor-Target-Host: clawvisor.local' \\",
		"    -H 'X-Clawvisor-Caller: Bearer cv-nonce-…' \\",
		"    http://localhost:25297/control/tasks ...",
		"",
		"✓ RIGHT — single curl, synthetic URL, JSON via heredoc:",
		"  curl -sS -X POST '" + tasksURLInline + "' \\",
		"    -H 'Content-Type: application/json' \\",
		"    --data @- <<'JSON'",
		"  {\"purpose\":\"<one-line user-visible goal>\",",
		"   \"expected_tools_json\":[{\"tool_name\":\"bash\",\"why\":\"<concrete reason>\"}],",
		"   \"intent_verification_mode\":\"strict\",",
		"   \"expires_in_seconds\":600}",
		"  JSON",
		"",
		"If you see `cv-nonce-…` or `X-Clawvisor-*` in your conversation history, that's a Clawvisor implementation detail — not a template to copy.",
		"",
		"Do NOT prefix tool calls with environment variables like `CLAWVISOR_TASK_ID=<id>`. Clawvisor tracks the active task server-side; it does not read that env var. Prefixing it on every command is harmless but unnecessary noise.",
	}, "\n")
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
	// elapses. The default tends to be ~1s, which is way shorter than
	// the task-creation curl's `wait=true&timeout=120` block window —
	// without clamping, the agent's task POST gets backgrounded and
	// the agent proceeds before the user can approve. Mention of
	// yield_time_ms in the prompt only makes the model cargo-cult a
	// small value back, so clamp here. Harmless on Bash (Claude Code
	// has no such parameter).
	clampControlToolUseTimeouts(raw)
	out, err := json.Marshal(raw)
	return out, true, err
}

// controlToolUseMinYieldMs is the floor we clamp Codex's
// exec_command yield_time_ms to when the call is a /control/* curl.
// The curl's max block is wait timeout (120s) plus network slop; 180s
// gives a comfortable margin without forcing the agent to wait
// substantially longer than necessary if the user replies quickly.
const controlToolUseMinYieldMs = 180_000

func clampControlToolUseTimeouts(raw map[string]any) {
	if raw == nil {
		return
	}
	// Codex's exec_command shape uses yield_time_ms.
	if cur, ok := numericFromAny(raw["yield_time_ms"]); ok {
		if cur < controlToolUseMinYieldMs {
			raw["yield_time_ms"] = controlToolUseMinYieldMs
		}
	} else if _, hasCmd := raw["cmd"]; hasCmd {
		// `cmd` field present + no yield_time_ms = Codex exec_command
		// using the harness default (~1s). Set the field explicitly so
		// the harness keeps the curl in the foreground long enough.
		raw["yield_time_ms"] = controlToolUseMinYieldMs
	}
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
// inline body. Any shape outside this allowlist (extra commands,
// pipes, subshells, variable expansion in paths, …) refuses closed.
func parseControlCmd(cmd string) ([]controlCurlArg, map[string][]byte, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil || len(file.Stmts) == 0 {
		return nil, nil, false
	}
	var curlStmt *syntax.Stmt
	dataFiles := map[string][]byte{}
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
			path, body, ok := parseHeredocToFile(stmt, call)
			if !ok {
				return nil, nil, false
			}
			dataFiles[path] = body
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
	if len(dataFiles) == 0 {
		dataFiles = nil
	}
	return args, dataFiles, true
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

// parseHeredocToFile recognizes
//
//	cat <<TAG >$staticpath
//	$body
//	TAG
//
// and returns ($staticpath, $body). Refuses any other cat-form
// (multiple redirs, pipes, dynamic path, missing heredoc).
func parseHeredocToFile(stmt *syntax.Stmt, call *syntax.CallExpr) (string, []byte, bool) {
	if len(call.Assigns) > 0 || len(call.Args) != 1 {
		return "", nil, false
	}
	// `cat` with no extra args — the heredoc + > redirect are the
	// shape we accept.
	if len(stmt.Redirs) < 2 {
		return "", nil, false
	}
	var heredocBody string
	var outPath string
	for _, redir := range stmt.Redirs {
		switch redir.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			if redir.Hdoc == nil {
				return "", nil, false
			}
			body, ok := staticShellWord(redir.Hdoc)
			if !ok {
				return "", nil, false
			}
			heredocBody = body
		case syntax.RdrOut, syntax.AppOut:
			path, ok := staticShellWord(redir.Word)
			if !ok || strings.TrimSpace(path) == "" {
				return "", nil, false
			}
			outPath = path
		default:
			return "", nil, false
		}
	}
	if outPath == "" || heredocBody == "" {
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
