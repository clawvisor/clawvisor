package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

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
	return nil, errors.New("inspector: no rewriter for tool input shape")
}

// ErrAmbiguous indicates the rewriter declined because the verdict was
// ambiguous or the call was classified as a non-API-call. The caller should
// emit a synthetic error block in the response stream.
var ErrAmbiguous = errors.New("inspector: ambiguous verdict, refusing to rewrite")

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
	raw["headers"] = headers

	out, err := json.Marshal(raw)
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
	cmdField := "cmd"
	cmdVal, ok := raw["cmd"].(string)
	if !ok {
		cmdField = "command"
		cmdVal, ok = raw["command"].(string)
	}
	if !ok || cmdVal == "" {
		return nil, false, nil
	}

	tokens, ok := simpleShellTokenize(cmdVal)
	if !ok || len(tokens) == 0 {
		return nil, false, nil
	}

	originalHost := v.Host
	rewroteAny := false
	for i, tok := range tokens {
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}
		parsed, err := url.Parse(tok)
		if err != nil || parsed.Host == "" {
			continue
		}
		if parsed.Hostname() != originalHost {
			continue
		}
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

	// Inject -H "X-Clawvisor-Target-Host: <originalHost>" as the *last*
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
	hostHeader := fmt.Sprintf("%s: %s", opts.TargetHostHeader, originalHost)
	injected := []string{"-H", hostHeader}
	if opts.CallerToken != "" && opts.CallerHeader != "" {
		callerHeader := fmt.Sprintf("%s: Bearer %s", opts.CallerHeader, opts.CallerToken)
		injected = append(injected, "-H", callerHeader)
	}
	tokens = append(tokens[:urlIdx],
		append(injected, tokens[urlIdx:]...)...)

	raw[cmdField] = joinShellTokens(tokens)
	out, err := json.Marshal(raw)
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
// single quotes via the standard '\'' trick.
func quoteShell(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\;|&<>()") {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
