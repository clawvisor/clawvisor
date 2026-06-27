package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/jsonpatch"
)

// ConversationIDHeader is the fixed header name the rewriter writes
// the per-turn conversation id into when RewriteOpts.ConversationID is
// non-empty. The control plane's /control/task/checkout handler reads
// this header so a manually-issued task checkout lands in the correct
// per-conversation bucket without the agent needing to put the
// conversation id in the request body itself.
const ConversationIDHeader = "X-Clawvisor-Conversation-ID"

// RewriteOpts controls how Rewrite produces the redirected tool_use input.
type RewriteOpts struct {
	// ResolverBaseURL is the URL the harness's eventual HTTP call should
	// land on (e.g. https://proxy.clawvisor.example). Method, path, query,
	// and headers are preserved; only the host and scheme are swapped to
	// point at the resolver.
	ResolverBaseURL string

	// TargetHostHeader is the header name the resolver reads to recover the
	// original target host. Defaults to "X-Clawvisor-Target-Host".
	TargetHostHeader string

	// CallerHeader is the header name the rewriter writes the caller-auth
	// token into so the harness's eventual HTTP call authenticates to the
	// resolver. Defaults to "X-Clawvisor-Caller".
	CallerHeader string

	// CallerToken is the raw `cvis_…` agent token. The rewriter writes
	// `Bearer <CallerToken>` into CallerHeader on the rewritten tool_use
	// so the harness's outbound HTTPS call to the resolver authenticates.
	// Required when ResolverBaseURL is set.
	//
	// SECURITY NOTE: this token becomes visible to the model on the next
	// turn (the harness echoes the rewritten tool_use back as part of
	// conversation history). Documented limitation; future work canonicalizes
	// the conversation history to strip it.
	CallerToken string

	// ConversationID is the per-turn conversation id resolved from the
	// inbound /v1/messages request. When non-empty the rewriter writes
	// it into ConversationIDHeader on the rewritten tool_use so the
	// control plane handlers can scope side effects (e.g. task
	// checkouts) to the correct conversation. Empty disables the
	// injection.
	ConversationID string
}

// DefaultRewriteOpts returns sensible defaults for production.
func DefaultRewriteOpts(resolverBaseURL string) RewriteOpts {
	return RewriteOpts{
		ResolverBaseURL:  strings.TrimRight(resolverBaseURL, "/"),
		TargetHostHeader: "X-Clawvisor-Target-Host",
		CallerHeader:     "X-Clawvisor-Caller",
	}
}

// Rewrite produces a new tool_use input JSON whose URL/Host has been
// redirected at the resolver. Returns the rewritten bytes; the caller
// substitutes this into the response stream in place of the original.
//
// Returns ErrAmbiguous if the verdict is ambiguous (caller should fail
// closed by replacing the tool_use with a synthetic error block).
func Rewrite(t ToolUse, v Verdict, opts RewriteOpts) ([]byte, error) {
	if v.Ambiguous || !v.IsAPICall {
		return nil, ErrAmbiguous
	}
	if opts.ResolverBaseURL == "" {
		return nil, errors.New("inspector: rewriter missing ResolverBaseURL")
	}
	if opts.TargetHostHeader == "" {
		opts.TargetHostHeader = "X-Clawvisor-Target-Host"
	}
	// CallerHeader has a documented default but the original code only
	// applied it via DefaultRewriteOpts. Manually-constructed RewriteOpts
	// with CallerToken set but CallerHeader empty would silently skip
	// caller-auth injection (the `opts.CallerToken != "" && opts.CallerHeader != ""`
	// guard fails when CallerHeader is empty). Apply the same default
	// here so any caller passing a token gets the header without having
	// to remember to set both fields.
	if opts.CallerHeader == "" {
		opts.CallerHeader = "X-Clawvisor-Caller"
	}

	resolverURL, err := url.Parse(opts.ResolverBaseURL)
	if err != nil {
		return nil, fmt.Errorf("inspector: parsing ResolverBaseURL %q: %w", opts.ResolverBaseURL, err)
	}

	// Dispatch by tool shape.
	if out, ok, err := rewriteStructured(t, v, resolverURL, opts); ok {
		return out, err
	}
	if out, ok, err := rewriteBash(t, v, resolverURL, opts); ok {
		return out, err
	}
	return nil, ErrNoRewriter
}

// ErrAmbiguous indicates the rewriter declined because the verdict was
// ambiguous or the call was classified as a non-API-call. The caller should
// emit a synthetic error block in the response stream.
var ErrAmbiguous = errors.New("inspector: ambiguous verdict, refusing to rewrite")

// ErrNoRewriter indicates the inspector classified the input as a
// credentialed API call but had no rewriter for the tool's input shape
// (e.g. a multi-statement shell script that wraps a credentialed curl
// in logic the bash rewriter can't safely parse). Callers should
// translate this into a synthetic tool result that points the agent at
// the autovault script-session path — that's the supported recovery
// route for shapes the rewriter declines.
var ErrNoRewriter = errors.New("inspector: no rewriter for tool input shape")

// rewriteStructured handles tools with a top-level `url` field.
func rewriteStructured(t ToolUse, _ Verdict, resolver *url.URL, opts RewriteOpts) ([]byte, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(t.Input, &raw); err != nil {
		return nil, false, nil
	}
	urlVal, ok := raw["url"].(string)
	if !ok || urlVal == "" {
		return nil, false, nil
	}
	parsed, err := url.Parse(urlVal)
	if err != nil || parsed.Host == "" {
		return nil, false, nil
	}

	rewritten := *parsed
	rewritten.Scheme = resolver.Scheme
	rewritten.Host = resolver.Host
	if resolver.Path != "" {
		rewritten.Path = strings.TrimRight(resolver.Path, "/") + parsed.Path
	}
	raw["url"] = rewritten.String()

	headers, _ := raw["headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{}
	}
	headers[opts.TargetHostHeader] = parsed.Host
	if opts.CallerToken != "" && opts.CallerHeader != "" {
		headers[opts.CallerHeader] = "Bearer " + opts.CallerToken
	}
	if opts.ConversationID != "" {
		headers[ConversationIDHeader] = opts.ConversationID
	}
	raw["headers"] = headers

	out, err := jsonpatch.MarshalNoEscape(raw)
	if err != nil {
		return nil, true, err
	}
	return out, true, nil
}

// rewriteBash handles `Bash`/`shell` tool inputs. Replaces the URL
// substring in the cmd with the resolver URL, and adds an extra `-H` flag
// with the original target host. v0 only supports the shapes the parser
// already recognized as safe; any structural change since parse-time
// (concurrent edits, etc.) returns false to fall back to ambiguous handling.
func rewriteBash(t ToolUse, v Verdict, resolver *url.URL, opts RewriteOpts) ([]byte, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(t.Input, &raw); err != nil {
		return nil, false, nil
	}
	// Some tools accept either `cmd` or `command`. An empty `cmd` should
	// fall through to `command` rather than aborting the rewrite — the
	// parser-side acceptance permits exactly this shape.
	cmdField := "cmd"
	cmdVal, ok := raw["cmd"].(string)
	if !ok || cmdVal == "" {
		if alt, altOK := raw["command"].(string); altOK && alt != "" {
			cmdField = "command"
			cmdVal = alt
			ok = true
		}
	}
	if !ok || cmdVal == "" {
		return nil, false, nil
	}

	// For compound commands (pipelines, chains, redirections), only
	// the credentialed simple-command needs rewriting; the rest of the
	// pipeline operates on output and must survive verbatim. Extract
	// the credentialed segment, rewrite that slice, and splice it back.
	seg, segErr := extractCredentialedCurlSegment(cmdVal)
	if segErr != "" || seg.text == "" {
		return nil, false, nil
	}

	tokens, ok := simpleShellTokenize(normalizeShellLineContinuations(seg.text))
	if !ok || len(tokens) == 0 {
		return nil, false, nil
	}

	// targetHost is the value we want the resolver to dial. v.Host is
	// hostname-only (parsed.Hostname()), used for the allowlist filter
	// at parse time. The *header* we emit must preserve any explicit
	// port from the original URL so a non-default port survives the
	// round-trip. We compute targetHost from the matched URL's parsed
	// Host (which is hostname+":port" when a port was specified, or
	// hostname-only otherwise).
	verdictHostname := v.Host
	targetHost := v.Host
	rewroteAny := false
	for i, tok := range tokens {
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}
		parsed, err := url.Parse(tok)
		if err != nil || parsed.Host == "" {
			continue
		}
		if parsed.Hostname() != verdictHostname {
			continue
		}
		// Use parsed.Host (may include :port) so an explicit port reaches
		// the resolver and downstream dial preserves it.
		targetHost = parsed.Host
		newURL := *parsed
		newURL.Scheme = resolver.Scheme
		newURL.Host = resolver.Host
		if resolver.Path != "" {
			newURL.Path = strings.TrimRight(resolver.Path, "/") + parsed.Path
		}
		tokens[i] = newURL.String()
		rewroteAny = true
		break // only one positional URL in v0
	}
	if !rewroteAny {
		return nil, false, nil
	}

	// Inject -H "X-Clawvisor-Target-Host: <targetHost>" as the *last*
	// flag before the URL. Simplest: append before the URL token.
	urlIdx := -1
	for i, tok := range tokens {
		if strings.HasPrefix(tok, resolver.Scheme+"://"+resolver.Host) {
			urlIdx = i
			break
		}
	}
	if urlIdx < 0 {
		urlIdx = len(tokens)
	}
	// Inject pre-shell-tokenized strings; joinShellTokens re-quotes each
	// at join time so values containing spaces (e.g. an Authorization
	// header value) survive the rejoin.
	hostHeader := fmt.Sprintf("%s: %s", opts.TargetHostHeader, targetHost)
	injected := []string{"-H", hostHeader}
	if opts.CallerToken != "" && opts.CallerHeader != "" {
		callerHeader := fmt.Sprintf("%s: Bearer %s", opts.CallerHeader, opts.CallerToken)
		injected = append(injected, "-H", callerHeader)
	}
	if opts.ConversationID != "" {
		convHeader := fmt.Sprintf("%s: %s", ConversationIDHeader, opts.ConversationID)
		injected = append(injected, "-H", convHeader)
	}
	tokens = append(tokens[:urlIdx],
		append(injected, tokens[urlIdx:]...)...)

	rewrittenSegment := joinShellTokens(tokens)
	// Splice the rewritten curl back into the original command,
	// preserving everything outside the credentialed sub-command
	// (pipes, redirects, neighboring simple commands).
	raw[cmdField] = cmdVal[:seg.start] + rewrittenSegment + cmdVal[seg.end:]
	out, err := jsonpatch.MarshalNoEscape(raw)
	if err != nil {
		return nil, true, err
	}
	return out, true, nil
}

// joinShellTokens rebuilds a shell command from a token slice. Each token
// is re-quoted via quoteShell so values containing whitespace or shell
// metacharacters survive the round-trip. simpleShellTokenize strips the
// original quotes; this is the symmetric re-emission step.
func joinShellTokens(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for i, tok := range tokens {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteShell(tok))
	}
	return b.String()
}

// quoteShell wraps a string in single quotes, escaping any embedded
// single quotes by closing, inserting an escaped quote, then reopening.
//
// Security: uses an ALLOW-list — only leaves a token unquoted when every
// character is known-safe (alphanumerics + a small URL-safe set). A
// deny-list misses metacharacters trivially (newline, tab, glob chars,
// brace/bracket expansion, comment marker), and a missed metacharacter
// can mean command injection via model-generated tool input.
func quoteShell(s string) string {
	if shellTokenSafe(s) {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellTokenSafe reports whether s contains only characters known to be
// safe to pass unquoted to /bin/sh-style shells. Conservative on purpose:
// any character not in the safe set forces quoting. Empty string is NOT
// safe (an unquoted empty token disappears from the argv), so callers
// want it quoted to preserve it as a discrete argument.
func shellTokenSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ',' ||
			r == ':' || r == '=' || r == '@' || r == '+' || r == '%':
		default:
			return false
		}
	}
	return true
}
