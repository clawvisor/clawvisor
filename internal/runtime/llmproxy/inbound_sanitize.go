package llmproxy

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// SanitizeInboundRequest configures inbound history sanitization. The
// resolver / control base URLs are the literal local daemon URLs that
// the rewriter substitutes IN; we reverse the substitution so the
// model only ever sees the synthetic URLs it originally emitted.
type SanitizeInboundRequest struct {
	Provider        conversation.Provider
	Body            []byte
	ResolverBaseURL string // e.g. "http://localhost:25297/proxy/v1"
	ControlBaseURL  string // e.g. "http://localhost:25297"
}

// SanitizeInboundResult reports the sanitized body and whether
// anything changed. When Modified is false the original Body is
// returned verbatim and callers can skip the re-parse step.
type SanitizeInboundResult struct {
	Body     []byte
	Modified bool
}

const ClawvisorManagedMarker = "[clawvisor-managed]"

// SanitizeInboundHistory walks an inbound /v1/messages or
// /v1/chat/completions request body and reverts proxy-rewritten
// transport details inside assistant tool_use blocks back to the
// synthetic form. Specifically, in every Bash-shaped tool_use we:
//
//   - Drop `-H 'X-Clawvisor-Caller: …'` and `-H 'X-Clawvisor-Target-Host: …'`
//     flags emitted by the rewriter.
//   - Convert `<daemon-resolver-base>/<path>` URLs back to
//     `https://<target-host>/<path>` (target host extracted from the
//     X-Clawvisor-Target-Host header before it was dropped).
//   - Convert `<daemon-control-base>/control/…` URLs back to the
//     synthetic `https://clawvisor.local/control/…` form.
//   - Replace any remaining literal `cv-nonce-…` substrings with a
//     non-secret marker so the model can't pattern-match from them.
//
// Models pattern-match aggressively from their own conversation
// history. Without this sanitization, after one rewrite turn the
// model sees `curl … http://localhost:25297/proxy/v1/user
// -H 'X-Clawvisor-Caller: Bearer cv-nonce-…' …` and starts emitting
// that shape directly on subsequent turns — bypassing the rewrite
// path and reusing stale nonces.
func SanitizeInboundHistory(req SanitizeInboundRequest) (SanitizeInboundResult, error) {
	if len(req.Body) == 0 {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	if !inboundLooksRewritten(req.Body) {
		// Cheap pre-filter: nothing rewritten in here → no-op.
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	switch req.Provider {
	case conversation.ProviderAnthropic:
		return sanitizeAnthropicInbound(req)
	case conversation.ProviderOpenAI:
		return sanitizeOpenAIInbound(req)
	default:
		return SanitizeInboundResult{Body: req.Body}, nil
	}
}

// inboundLooksRewritten is a quick substring check before paying the
// JSON-walk cost. We only need to sanitize when at least one of the
// rewriter's calling-cards is present.
func inboundLooksRewritten(body []byte) bool {
	return bytes.Contains(body, []byte("cv-nonce-")) ||
		bytes.Contains(body, []byte("X-Clawvisor-Caller")) ||
		bytes.Contains(body, []byte("X-Clawvisor-Target-Host"))
}

// sanitizeAnthropicInbound walks messages[].content[] looking for
// tool_use blocks and rewrites the inner cmd / command field.
func sanitizeAnthropicInbound(req SanitizeInboundRequest) (SanitizeInboundResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &raw); err != nil {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	modified := false
	for _, msg := range messages {
		roleRaw := msg["role"]
		var role string
		_ = json.Unmarshal(roleRaw, &role)
		if role != "assistant" {
			continue
		}
		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &blocks); err != nil {
			continue
		}
		blockChanged := false
		for _, block := range blocks {
			typeRaw := block["type"]
			var typ string
			_ = json.Unmarshal(typeRaw, &typ)
			if typ != "tool_use" {
				continue
			}
			inputRaw, ok := block["input"]
			if !ok {
				continue
			}
			sanitized, changed := sanitizeToolUseInput(inputRaw, req)
			if changed {
				block["input"] = sanitized
				blockChanged = true
			}
		}
		if blockChanged {
			encoded, err := json.Marshal(blocks)
			if err == nil {
				msg["content"] = encoded
				modified = true
			}
		}
	}
	if !modified {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	encodedMsgs, err := json.Marshal(messages)
	if err != nil {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	raw["messages"] = encodedMsgs
	out, err := json.Marshal(raw)
	if err != nil {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	return SanitizeInboundResult{Body: out, Modified: true}, nil
}

// sanitizeOpenAIInbound walks messages[].tool_calls[].function.arguments
// (Chat Completions) and input[].arguments (Responses API). Both carry
// the bash command as a JSON-encoded string inside `arguments`.
func sanitizeOpenAIInbound(req SanitizeInboundRequest) (SanitizeInboundResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &raw); err != nil {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	modified := false
	if encoded, changed := sanitizeOpenAIChatMessages(raw["messages"], req); changed {
		raw["messages"] = encoded
		modified = true
	}
	if encoded, changed := sanitizeOpenAIResponseInput(raw["input"], req); changed {
		raw["input"] = encoded
		modified = true
	}
	if !modified {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return SanitizeInboundResult{Body: req.Body}, nil
	}
	return SanitizeInboundResult{Body: out, Modified: true}, nil
}

func sanitizeOpenAIChatMessages(messagesRaw json.RawMessage, req SanitizeInboundRequest) (json.RawMessage, bool) {
	if len(messagesRaw) == 0 {
		return nil, false
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, false
	}
	changed := false
	for _, msg := range messages {
		// Only assistant turns can legitimately carry rewriter
		// transport details (the rewriter only mutates the
		// assistant's tool_use blocks). A non-assistant message
		// with a `tool_calls` field is at best malformed or at
		// worst an attempt to feed back something model-shaped; in
		// either case we don't sanitize it.
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "assistant" {
			continue
		}
		callsRaw, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		var calls []map[string]json.RawMessage
		if err := json.Unmarshal(callsRaw, &calls); err != nil {
			continue
		}
		callsChanged := false
		for _, call := range calls {
			fnRaw, ok := call["function"]
			if !ok {
				continue
			}
			var fn map[string]json.RawMessage
			if err := json.Unmarshal(fnRaw, &fn); err != nil {
				continue
			}
			argsRaw, ok := fn["arguments"]
			if !ok {
				continue
			}
			var argsStr string
			if err := json.Unmarshal(argsRaw, &argsStr); err != nil {
				continue
			}
			sanitized, mut := sanitizeArgumentsJSONString(argsStr, req)
			if !mut {
				continue
			}
			encoded, err := json.Marshal(sanitized)
			if err != nil {
				continue
			}
			fn["arguments"] = encoded
			fnEncoded, err := json.Marshal(fn)
			if err != nil {
				continue
			}
			call["function"] = fnEncoded
			callsChanged = true
		}
		if callsChanged {
			encoded, err := json.Marshal(calls)
			if err == nil {
				msg["tool_calls"] = encoded
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(messages)
	if err != nil {
		return nil, false
	}
	return out, true
}

// sanitizeOpenAIResponseInput walks the Responses-API `input` array.
// Function-call items live alongside text/message items; only the
// function_call shape carries an `arguments` field we need to mutate.
func sanitizeOpenAIResponseInput(inputRaw json.RawMessage, req SanitizeInboundRequest) (json.RawMessage, bool) {
	if len(inputRaw) == 0 {
		return nil, false
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, false
	}
	changed := false
	for _, item := range items {
		typeRaw := item["type"]
		var typ string
		_ = json.Unmarshal(typeRaw, &typ)
		if typ != "function_call" && typ != "custom_tool_call" {
			continue
		}
		argsRaw, ok := item["arguments"]
		if !ok {
			argsRaw, ok = item["input"]
			if !ok {
				continue
			}
		}
		var argsStr string
		if err := json.Unmarshal(argsRaw, &argsStr); err != nil {
			continue
		}
		sanitized, mut := sanitizeArgumentsJSONString(argsStr, req)
		if !mut {
			continue
		}
		encoded, err := json.Marshal(sanitized)
		if err != nil {
			continue
		}
		if _, has := item["arguments"]; has {
			item["arguments"] = encoded
		} else {
			item["input"] = encoded
		}
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil, false
	}
	return out, true
}

// sanitizeArgumentsJSONString parses the (string-encoded) function
// arguments, applies sanitizeToolUseInput to it, and returns the new
// JSON-encoded string. Used by the OpenAI provider paths where args
// arrive as a serialized JSON blob, not as a structured object.
func sanitizeArgumentsJSONString(argsStr string, req SanitizeInboundRequest) (string, bool) {
	if !strings.Contains(argsStr, "cv-nonce-") &&
		!strings.Contains(argsStr, "X-Clawvisor-Caller") &&
		!strings.Contains(argsStr, "X-Clawvisor-Target-Host") {
		return argsStr, false
	}
	sanitized, changed := sanitizeToolUseInput(json.RawMessage(argsStr), req)
	if !changed {
		return argsStr, false
	}
	return string(sanitized), true
}

// sanitizeToolUseInput mutates a single tool_use input JSON object.
// It only touches the cmd / command field; other fields pass through
// verbatim. Returns the rewritten JSON bytes and whether anything
// changed.
func sanitizeToolUseInput(inputRaw json.RawMessage, req SanitizeInboundRequest) (json.RawMessage, bool) {
	if len(inputRaw) == 0 {
		return inputRaw, false
	}
	var input map[string]any
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return inputRaw, false
	}
	changed := false
	for _, field := range []string{"cmd", "command"} {
		val, ok := input[field].(string)
		if !ok || val == "" {
			continue
		}
		sanitized, mut := sanitizeBashCommand(val, req)
		if mut {
			input[field] = sanitized
			changed = true
		}
	}
	if !changed {
		return inputRaw, false
	}
	out, err := json.Marshal(input)
	if err != nil {
		return inputRaw, false
	}
	return out, true
}

var (
	// reCallerHeader matches `-H ' X-Clawvisor-Caller: …'` and its
	// double-quoted variant. The rewriter emits single-quoted; allow
	// both for defensive parsing.
	reCallerHeader = regexp.MustCompile(`\s*-H\s+(?:'X-Clawvisor-Caller:[^']*'|"X-Clawvisor-Caller:[^"]*")`)
	// reTargetHeader captures the host so we can use it to revert the
	// rewritten URL. Captures the host value in group 1.
	reTargetHeader = regexp.MustCompile(`\s*-H\s+(?:'X-Clawvisor-Target-Host:\s*([^']*)'|"X-Clawvisor-Target-Host:\s*([^"]*)")`)
	// reNonceLeftover catches any cv-nonce-… that survived
	// header-stripping (e.g. accidentally embedded outside a -H flag).
	reNonceLeftover = regexp.MustCompile(`cv-nonce-[A-Za-z0-9]+`)
)

// sanitizeBashCommand applies the four sanitization steps to a single
// bash command string. Idempotent — running it twice yields the same
// result.
func sanitizeBashCommand(cmd string, req SanitizeInboundRequest) (string, bool) {
	original := cmd
	// Pull the target host out before dropping the header so we can
	// use it to revert the URL. Multiple matches are possible if the
	// model emitted the header more than once; the first wins.
	targetHost := ""
	if m := reTargetHeader.FindStringSubmatch(cmd); m != nil {
		if m[1] != "" {
			targetHost = m[1]
		} else {
			targetHost = m[2]
		}
	}
	// Strip the proxy headers.
	cmd = reTargetHeader.ReplaceAllString(cmd, "")
	cmd = reCallerHeader.ReplaceAllString(cmd, "")

	// Revert URLs. The resolver rewrite is `<resolver>/<path>` where
	// resolver is something like `http://localhost:25297/proxy/v1`.
	// The control rewrite is `<daemon>/control/<path>`.
	resolverBase := strings.TrimRight(req.ResolverBaseURL, "/")
	controlBase := strings.TrimRight(req.ControlBaseURL, "/")
	if resolverBase != "" {
		cmd = revertResolverURLs(cmd, resolverBase, targetHost)
	}
	if controlBase != "" {
		cmd = revertControlURLs(cmd, controlBase)
	}

	// Drop any cv-nonce-… leftovers (e.g. a model that pasted the
	// nonce into a description or body field).
	cmd = reNonceLeftover.ReplaceAllString(cmd, ClawvisorManagedMarker)

	// Collapse double-spaces left over from header removal.
	cmd = collapseSpaces(cmd)

	return cmd, cmd != original
}

// revertResolverURLs replaces every <resolver>/<path> URL inside cmd
// with https://<targetHost>/<path>. When targetHost is empty (no
// header found) we use a placeholder to avoid emitting a half-valid
// URL the model might try to copy.
func revertResolverURLs(cmd, resolverBase, targetHost string) string {
	for {
		idx := strings.Index(cmd, resolverBase)
		if idx < 0 {
			return cmd
		}
		end := idx + len(resolverBase)
		// Capture the path that follows, stopping at whitespace, quote,
		// or end-of-string.
		pathEnd := end
		for pathEnd < len(cmd) {
			c := cmd[pathEnd]
			if c == ' ' || c == '\'' || c == '"' || c == '\n' || c == '\t' {
				break
			}
			pathEnd++
		}
		path := cmd[end:pathEnd]
		replacement := "https://" + ClawvisorManagedMarker + path
		if targetHost != "" {
			// Drop leading "/" so url.Parse doesn't choke.
			if u := buildSyntheticURL(targetHost, path); u != "" {
				replacement = u
			}
		}
		cmd = cmd[:idx] + replacement + cmd[pathEnd:]
	}
}

func revertControlURLs(cmd, controlBase string) string {
	// Replace `<controlBase>/control/…` and `<controlBase>/proxy/v1`
	// has already been handled by revertResolverURLs. Here we only
	// reach this when the rewriter targeted the control plane.
	target := controlBase + "/control/"
	for {
		idx := strings.Index(cmd, target)
		if idx < 0 {
			return cmd
		}
		end := idx + len(target)
		pathEnd := end
		for pathEnd < len(cmd) {
			c := cmd[pathEnd]
			if c == ' ' || c == '\'' || c == '"' || c == '\n' || c == '\t' {
				break
			}
			pathEnd++
		}
		path := cmd[end:pathEnd]
		cmd = cmd[:idx] + "https://clawvisor.local/control/" + path + cmd[pathEnd:]
	}
}

// buildSyntheticURL combines a target host (possibly with port) and a
// path. Returns "" when the host doesn't parse — caller falls back to
// the placeholder.
func buildSyntheticURL(host, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// Defensive: prevent a model-emitted target host of "evil.example
	// /malicious-path" from injecting into the reverted URL.
	if strings.ContainsAny(host, " \t\n'\"") {
		return ""
	}
	u := &url.URL{Scheme: "https", Host: host, Path: ""}
	return u.String() + path
}

// collapseSpaces collapses runs of multiple spaces left after header
// removal. Tabs/newlines are preserved.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			if lastSpace {
				continue
			}
			lastSpace = true
			b.WriteByte(c)
			continue
		}
		lastSpace = false
		b.WriteByte(c)
	}
	return b.String()
}
