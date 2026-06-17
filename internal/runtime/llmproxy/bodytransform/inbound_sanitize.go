package bodytransform

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
)

// SanitizeInboundRequest configures inbound history sanitization. The
// resolver / control base URLs are the literal local daemon URLs that
// the rewriter substitutes IN; we reverse the substitution so the
// model only ever sees the synthetic URLs it originally emitted.
type SanitizeInboundRequest struct {
	Provider        conversation.Provider
	Body            []byte
	ResolverBaseURL string // e.g. "http://localhost:25297/api/proxy"
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

// SanitizeInboundHistory walks an inbound /api/v1/messages or
// /api/v1/chat/completions request body and reverts proxy-rewritten
// transport details inside assistant tool_use blocks back to the
// synthetic form. Specifically, in every Bash-shaped tool_use we:
//
//   - Drop `-H 'X-Clawvisor-Caller: …'` and `-H 'X-Clawvisor-Target-Host: …'`
//     flags emitted by the rewriter.
//   - Convert `<daemon-resolver-base>/<path>` URLs back to
//     `https://<target-host>/<path>` (target host extracted from the
//     X-Clawvisor-Target-Host header before it was dropped).
//   - Convert `<daemon-control-base>/api/control/…` URLs back to the
//     synthetic `https://clawvisor.local/control/…` form.
//   - Replace any remaining literal `cv-nonce-…` substrings with a
//     non-secret marker so the model can't pattern-match from them.
//
// Models pattern-match aggressively from their own conversation
// history. Without this sanitization, after one rewrite turn the
// model sees `curl … http://localhost:25297/api/proxy/user
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
//
// Byte fidelity invariant: unchanged messages and unchanged content
// blocks pass through with their original bytes intact. Top-level
// body keys, message keys, and block keys keep their incoming order.
// Critical because Anthropic verifies thinking-block signatures across
// turns; any reshape on the request path can corrupt them.
func sanitizeAnthropicInbound(req SanitizeInboundRequest) (SanitizeInboundResult, error) {
	body := req.Body
	msgsStart, msgsEnd, ok := jsonsurgery.FindFieldValue(body, "messages")
	if !ok {
		return SanitizeInboundResult{Body: body}, nil
	}
	messages, ok := jsonsurgery.FlattenArray(body[msgsStart:msgsEnd])
	if !ok {
		return SanitizeInboundResult{Body: body}, nil
	}
	anyChanged := false
	newMessages := make([]json.RawMessage, len(messages))
	for i, msg := range messages {
		newMsg, changed := sanitizeAnthropicInboundMessage(msg, req)
		newMessages[i] = newMsg
		if changed {
			anyChanged = true
		}
	}
	if !anyChanged {
		return SanitizeInboundResult{Body: body}, nil
	}
	newMsgsBytes, err := jsonsurgery.MarshalNoEscape(newMessages)
	if err != nil {
		return SanitizeInboundResult{Body: body}, nil
	}
	out, err := jsonsurgery.SetField(body, "messages", newMsgsBytes)
	if err != nil {
		return SanitizeInboundResult{Body: body}, nil
	}
	return SanitizeInboundResult{Body: out, Modified: true}, nil
}

// sanitizeAnthropicInboundMessage rewrites a single message's
// assistant tool_use input commands while preserving byte fidelity
// for everything else (other blocks, the message envelope).
func sanitizeAnthropicInboundMessage(msg json.RawMessage, req SanitizeInboundRequest) (json.RawMessage, bool) {
	roleStart, roleEnd, ok := jsonsurgery.FindFieldValue(msg, "role")
	if !ok {
		return msg, false
	}
	var role string
	if err := json.Unmarshal(msg[roleStart:roleEnd], &role); err != nil || role != "assistant" {
		return msg, false
	}
	contentStart, contentEnd, ok := jsonsurgery.FindFieldValue(msg, "content")
	if !ok {
		return msg, false
	}
	content := msg[contentStart:contentEnd]
	blocks, ok := jsonsurgery.FlattenArray(content)
	if !ok {
		return msg, false
	}
	anyChanged := false
	newBlocks := make([]json.RawMessage, len(blocks))
	for i, block := range blocks {
		if extractBlockType(block) != "tool_use" {
			newBlocks[i] = block
			continue
		}
		inputStart, inputEnd, ok := jsonsurgery.FindFieldValue(block, "input")
		if !ok {
			newBlocks[i] = block
			continue
		}
		sanitized, changed := sanitizeToolUseInput(block[inputStart:inputEnd], req)
		if !changed {
			newBlocks[i] = block
			continue
		}
		newBlock, err := jsonsurgery.SetField(block, "input", sanitized)
		if err != nil {
			newBlocks[i] = block
			continue
		}
		newBlocks[i] = newBlock
		anyChanged = true
	}
	if !anyChanged {
		return msg, false
	}
	newContentBytes, err := jsonsurgery.MarshalNoEscape(newBlocks)
	if err != nil {
		return msg, false
	}
	newMsg, err := jsonsurgery.SetField(msg, "content", newContentBytes)
	if err != nil {
		return msg, false
	}
	return newMsg, true
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
	out, err := jsonsurgery.MarshalNoEscape(raw)
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
			encoded, err := jsonsurgery.MarshalNoEscape(sanitized)
			if err != nil {
				continue
			}
			fn["arguments"] = encoded
			fnEncoded, err := jsonsurgery.MarshalNoEscape(fn)
			if err != nil {
				continue
			}
			call["function"] = fnEncoded
			callsChanged = true
		}
		if callsChanged {
			encoded, err := jsonsurgery.MarshalNoEscape(calls)
			if err == nil {
				msg["tool_calls"] = encoded
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := jsonsurgery.MarshalNoEscape(messages)
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
		encoded, err := jsonsurgery.MarshalNoEscape(sanitized)
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
	out, err := jsonsurgery.MarshalNoEscape(items)
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
	out, err := jsonsurgery.MarshalNoEscape(input)
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
	// resolver is something like `http://localhost:25297/api/proxy`.
	// The control rewrite is `<daemon>/api/control/<path>`.
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
	// Replace `<controlBase>/api/control/…` and `<controlBase>/api/proxy`
	// has already been handled by revertResolverURLs. Here we only
	// reach this when the rewriter targeted the control plane.
	target := controlBase + "/api/control/"
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
	// The model emits the target host into a header the rewriter then
	// reads back at sanitize time. Without a strict validation here, a
	// host like `evil.com/../legit.com` survives the whitespace check
	// and the resulting URL `https://evil.com/../legit.com/<path>` ends
	// up in the assistant history shown back to the model. The host is
	// for visualization only (no outbound call is built from this
	// path) but loose validation invites confusion and signature
	// drift. Restrict to the production DNS character set: ASCII
	// letters / digits / `.` / `-` / `_`, plus an optional `:port`
	// suffix and bracketed IPv6 literals.
	if host == "" || !isValidSyntheticHost(host) {
		return ""
	}
	u := &url.URL{Scheme: "https", Host: host, Path: ""}
	return u.String() + path
}

// isValidSyntheticHost reports whether host is a syntactically plausible
// hostname[:port] or [v6literal][:port]. The accepted character set
// is intentionally narrower than RFC 1035 — production targets do not
// contain `/`, `?`, `#`, `@`, `%`, etc. and accepting them lets a
// model-emitted host smuggle path components into the surrounding URL
// reconstruction.
func isValidSyntheticHost(host string) bool {
	if len(host) > 253 {
		return false
	}
	// Bracketed IPv6: validate inside the brackets and allow trailing
	// :port outside.
	if host[0] == '[' {
		end := strings.IndexByte(host, ']')
		if end < 2 {
			return false
		}
		inner := host[1:end]
		// Inner must look like an IPv6 literal: hex digits, dots, and
		// colons only.
		for i := 0; i < len(inner); i++ {
			c := inner[i]
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex && c != ':' && c != '.' {
				return false
			}
		}
		tail := host[end+1:]
		if tail == "" {
			return true
		}
		if tail[0] != ':' {
			return false
		}
		return isValidPort(tail[1:])
	}
	// Hostname (or IPv4) with optional :port.
	hostPart := host
	portPart := ""
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		hostPart = host[:i]
		portPart = host[i+1:]
	}
	if hostPart == "" {
		return false
	}
	for i := 0; i < len(hostPart); i++ {
		c := hostPart[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	if portPart != "" && !isValidPort(portPart) {
		return false
	}
	return true
}

func isValidPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
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
