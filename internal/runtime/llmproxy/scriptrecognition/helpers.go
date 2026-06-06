package scriptrecognition

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
	"mvdan.cc/sh/v3/syntax"
)

// ScriptSessionPrefix is the leading byte sequence of every script-session token.
const ScriptSessionPrefix = "cv-script-"

// scriptSessionToolUse reports whether a tool_use input looks like a
// legitimate script-session call: a curl (or structured tool_use) that
// carries a cv-script-prefixed caller token AND targets our resolver
// mount. When true the inspector is skipped — the resolver enforces
// session scope on every actual request.
//
// The threat model accepts that the AGENT could construct mischief
// with their cv-script token (use --proxy attacker, pipe to a remote
// sink, etc.). The mitigation for that lives at the mint-time intent
// verifier, which evaluates the agent's stated `why` against the
// task's purpose before issuing the token, and the resolver, which
// enforces scope on every actual request. Parser-level checks on the
// agent's curl shape (single-curl-only, no variable expansion, flag
// allowlists) don't add real defense — the agent could use any of
// python/node/perl/etc. to achieve the same effect, and the parser
// only knows curl. The asymmetry was creating friction (rejecting
// legitimate `while read id; do curl …/${id}; done` loops) without a
// matching security benefit.
//
// What this function still enforces:
//   - a cv-script-prefixed token must appear at the X-Clawvisor-Caller
//     header position (so we don't skip the inspector on a tool_use
//     that merely mentions the prefix in a string literal), AND
//   - at least one curl URL literal prefix must target our resolver
//     mount (host:port + path-prefix, with traversal rejection). This
//     is recognition, not enforcement: if the URL doesn't look like
//     ours, we let the inspector run as usual; we're not claiming the
//     call is safe.
//
// resolverBaseURL is the proxy's /api/proxy mount (e.g.
// "http://localhost:25297/api/proxy"). Empty disables passthrough.
// ScriptSessionToolUse reports whether a tool_use is the agent's
// already-shaped script-session call (cv-script caller token + URL
// targeting our resolver mount). Used by the
// policies.ScriptSessionEvaluator.
func ScriptSessionToolUse(input json.RawMessage, resolverBaseURL string) bool {
	if len(input) == 0 || resolverBaseURL == "" {
		return false
	}
	proxyHost, proxyPath := resolverPassthroughTarget(resolverBaseURL)
	if proxyHost == "" {
		return false
	}
	var raw struct {
		Headers map[string]json.RawMessage `json:"headers,omitempty"`
		URL     string                     `json:"url,omitempty"`
		Cmd     string                     `json:"cmd,omitempty"`
		Command string                     `json:"command,omitempty"`
	}
	if err := json.Unmarshal(input, &raw); err != nil {
		return false
	}
	// Structured tool shape: top-level `url` + `headers` map.
	if headerHasScriptSessionToken(raw.Headers) && urlTargetsResolver(raw.URL, proxyHost, proxyPath) {
		return true
	}
	// Bash/exec shape: walk the parsed cmd for any curl invocation
	// with a cv-script caller header AND a URL whose literal prefix
	// targets our resolver mount. Variable expansion after the
	// literal prefix is fine — the resolver enforces scope on the
	// actual expanded URL. Pipelines, multi-statement scripts,
	// redirects, and additional shell wrappers are all allowed; the
	// resolver is the perimeter.
	cmd := raw.Cmd
	if cmd == "" {
		cmd = raw.Command
	}
	if cmd == "" {
		return false
	}
	urls, headers := extractCurlIntent(cmd)
	if len(urls) == 0 || len(headers) == 0 {
		return false
	}
	if !headerValuesHaveScriptSessionToken(headers) {
		return false
	}
	for _, u := range urls {
		if urlTargetsResolver(u, proxyHost, proxyPath) {
			return true
		}
	}
	return false
}

// headerValuesHaveScriptSessionToken reports whether any
// X-Clawvisor-Caller header value (any form: `-H Name: value`,
// `--header Name: value`, `--header=Name: value`) carries a
// script-session token.
func headerValuesHaveScriptSessionToken(headers []string) bool {
	for _, h := range headers {
		name, value, ok := splitHeaderArg(h)
		if !ok {
			continue
		}
		if !strings.EqualFold(name, "X-Clawvisor-Caller") {
			continue
		}
		if hasScriptSessionToken(value) {
			return true
		}
	}
	return false
}

// splitHeaderArg parses a curl -H/--header value of the form
// "Name: value" into (name, value, true). Returns false on shapes
// that don't fit (no colon, empty name).
func splitHeaderArg(raw string) (name, value string, ok bool) {
	i := strings.IndexByte(raw, ':')
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:]), true
}

// extractCurlIntent walks a bash command's AST for ANY curl invocation
// (across pipelines, multi-statement scripts, subshells, while-loops,
// xargs/parallel/find-exec/sh -c wrappers, etc.) and returns the URL
// literal-prefixes + -H/--header values it finds. "Literal prefix"
// means the leading static portion of each arg — variable expansion,
// command substitution, etc. just cut the prefix short rather than
// disqualify the arg.
//
// Best-effort by design: this is recognition for the script-session
// passthrough, not security enforcement. The mint-time intent verifier
// and the resolver's per-request scope check are the actual gates;
// this function only decides "does this tool_use look like a legit
// script-session call to our resolver, so we can skip the inspector?"
//
// Parse errors return empty slices — caller treats that as "no match,"
// and the inspector runs as usual.
func extractCurlIntent(cmd string) (urls []string, headers []string) {
	extractCurlIntentInto(cmd, &urls, &headers, 0)
	return urls, headers
}

// extractCurlIntentMaxDepth bounds the recursion through `sh -c` /
// `bash -c` wrappers. Three is enough for any realistic agent
// pattern (`xargs … sh -c '… sh -c "…"'` would already be
// pathological); it prevents accidental runaway from crafted inputs.
const extractCurlIntentMaxDepth = 3

func extractCurlIntentInto(cmd string, urls, headers *[]string, depth int) {
	if depth > extractCurlIntentMaxDepth {
		return
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		// Extract from any CallExpr's args — not just ones where
		// the head is "curl". Real script patterns wrap curl in
		// xargs / parallel / find -exec / shell functions, all of
		// which put "curl" somewhere other than args[0]. As long
		// as a URL targeting our resolver and a cv-script -H both
		// appear in the same arg list, recognition is fine — the
		// resolver still enforces on the actual request.
		extractFromCurlArgs(call.Args, urls, headers, depth)
		return true
	})
}

// isShellInvocation reports whether the given literal could be the
// invocation of a POSIX-ish shell that takes a `-c <command>` arg
// whose value is itself a shell script. Used to detect the
// `xargs … sh -c '…'`, `bash -c '…'`, `find . -exec sh -c '…' \;`
// patterns so we can recurse into the nested script.
func isShellInvocation(prefix string) bool {
	switch prefix {
	case "sh", "bash", "zsh", "dash", "ash", "ksh",
		"/bin/sh", "/bin/bash", "/bin/zsh", "/bin/dash",
		"/usr/bin/sh", "/usr/bin/bash", "/usr/bin/env":
		return true
	}
	return false
}

// extractFromCurlArgs collects URL literal-prefixes and -H/--header
// values from a single curl call's arg list. It handles space-
// separated (`-H "X: y"`) and equals-attached (`--header=X: y`) forms
// for headers, and treats any non-flag positional starting with
// http:// or https:// as a URL. When it sees a `(sh|bash) -c <arg>`
// pattern, it recursively parses <arg> as bash to discover curl
// invocations nested inside `xargs … sh -c '…'` and similar shapes.
func extractFromCurlArgs(args []*syntax.Word, urls, headers *[]string, depth int) {
	for i := 0; i < len(args); i++ {
		prefix := controltool.ShellWordLiteralPrefix(args[i])

		// `--header=value` / `--url=value` form: literal prefix
		// includes the flag name + `=` + the start of the value.
		if strings.HasPrefix(prefix, "--header=") {
			*headers = append(*headers, prefix[len("--header="):])
			continue
		}
		if strings.HasPrefix(prefix, "--url=") {
			candidate := prefix[len("--url="):]
			if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
				*urls = append(*urls, candidate)
			}
			continue
		}

		// Flag-then-value form: `-H value`, `--header value`,
		// `--url value`. Only headers + url need value capture;
		// other flags are ignored.
		if prefix == "-H" || prefix == "--header" {
			if i+1 < len(args) {
				*headers = append(*headers, controltool.ShellWordLiteralPrefix(args[i+1]))
				i++
			}
			continue
		}
		if prefix == "--url" {
			if i+1 < len(args) {
				candidate := controltool.ShellWordLiteralPrefix(args[i+1])
				if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
					*urls = append(*urls, candidate)
				}
				i++
			}
			continue
		}

		// Any other flag — skip without value capture. We
		// deliberately don't track flag-arity for non-header/url
		// flags; over-capturing a "value" as a URL is fine because
		// the http:// / https:// prefix check filters it out, and
		// over-capturing a value as a separate arg is harmless
		// since we don't enforce anything about extra args.
		if strings.HasPrefix(prefix, "-") {
			continue
		}

		// Shell-spawning pattern: `(sh|bash) -c <script>`. The
		// nested <script> is itself a bash command that may contain
		// the curl invocation. Common forms we want to support:
		//   xargs -I {} sh -c 'curl …'
		//   bash -c 'curl …'
		//   find . -exec sh -c 'curl …' \;
		// Look ahead for `-c <next>` and recurse into <next>.
		// We do this OPPORTUNISTICALLY — even if recursion finds
		// nothing useful, we still let the surrounding loop handle
		// this arg as a positional below.
		if isShellInvocation(prefix) {
			for j := i + 1; j < len(args)-1; j++ {
				if controltool.ShellWordLiteralPrefix(args[j]) == "-c" {
					nested := controltool.ShellWordLiteralPrefix(args[j+1])
					if nested != "" {
						extractCurlIntentInto(nested, urls, headers, depth+1)
					}
					break
				}
			}
		}

		// Positional. If it parses as a URL, record the literal
		// prefix — enough for urlTargetsResolver to confirm the
		// resolver host + path-prefix even when a suffix like
		// `${id}` expands at runtime.
		if strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") {
			*urls = append(*urls, prefix)
		}
	}
}

// resolverPassthroughTarget returns the (host:port, path-prefix) pair
// we require passthrough curls to target. Empty host disables
// passthrough — the caller should treat that as "no match." The path
// prefix has any trailing slash stripped so the urlTargetsResolver
// caller can apply its own "/"-or-exact-equality boundary rule.
func resolverPassthroughTarget(baseURL string) (host, pathPrefix string) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", ""
	}
	return u.Host, strings.TrimRight(u.Path, "/")
}

// headerHasScriptSessionToken reports whether the JSON-decoded headers
// map carries a ScriptSession-shaped value at X-Clawvisor-Caller.
func headerHasScriptSessionToken(headers map[string]json.RawMessage) bool {
	for k, v := range headers {
		if !strings.EqualFold(k, "X-Clawvisor-Caller") {
			continue
		}
		var val string
		if err := json.Unmarshal(v, &val); err != nil {
			continue
		}
		if hasScriptSessionToken(val) {
			return true
		}
	}
	return false
}

// urlTargetsResolver reports whether the URL points at our resolver:
// host:port matches AND the path falls under the resolver mount
// (e.g. "/api/proxy"). Path-prefix matching matters because a
// host-only check would let the passthrough fire for
// http://proxy-host/admin/whatever — same host, but the agent's curl
// would skip the inspector while routing somewhere that isn't the
// resolver at all. Empty / unparseable URLs are not matches.
//
// The boundary rule is "exact prefix or prefix + '/'", so
// "/api/proxy/foo" matches but "/api/proxyfoo" does NOT. An empty
// pathPrefix degenerates to "any path on the host", which is the
// correct behavior when the configured resolver base has no path
// component.
func urlTargetsResolver(rawURL, proxyHost, pathPrefix string) bool {
	if rawURL == "" || proxyHost == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Host, proxyHost) {
		return false
	}
	// Reject traversal-shaped paths BEFORE the prefix check. A literal
	// "/api/proxy/../admin/x" satisfies HasPrefix on "/api/proxy/" but
	// resolves to "/admin/x" after server-side normalization — so the
	// passthrough would skip the inspector for a URL that doesn't
	// actually hit the resolver. Percent-encoded forms (%2e%2e, etc.)
	// matter for downstream decoders that normalize differently than
	// net/url; checking EscapedPath catches both shapes.
	if pathHasTraversal(u.Path) || pathHasTraversal(u.EscapedPath()) {
		return false
	}
	if pathPrefix == "" {
		return true
	}
	p := u.Path
	if p == pathPrefix {
		return true
	}
	return strings.HasPrefix(p, pathPrefix+"/")
}

// hasScriptSessionToken reports whether v is a script-session caller-
// auth value: a ScriptSessionPrefix-prefixed token, optionally wrapped
// in `Bearer ` (case-sensitive — Anthropic + OpenAI both use that
// exact casing, and we don't want to encourage weirder forms).
func hasScriptSessionToken(v string) bool {
	v = strings.TrimSpace(v)
	const bearer = "Bearer "
	if strings.HasPrefix(v, bearer) {
		v = strings.TrimSpace(v[len(bearer):])
	}
	return strings.HasPrefix(v, ScriptSessionPrefix)
}
