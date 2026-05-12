package inspector

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/clawvisor/clawvisor/internal/runtime/autovault"
)

// DefaultParser handles the day-one supported tool shapes:
//
//   - Structured fetch tools that declare a top-level `url` field
//     (`WebFetch`, `fetch`, `http_request`, and aliases).
//   - `Bash` / `shell` with a single leading curl-shaped command. The
//     v0 parser handles the simple cases (single URL positional arg,
//     -H header flags, -X method) via shell-quote-aware tokenization.
//     Pathological commands fall through to the validator (which will
//     return ambiguous and the rewriter will fail closed).
//
// Anything outside these shapes returns (zero, false) so the inspector
// falls through to the LLM validator.
type DefaultParser struct{}

// Parse implements Parser.
func (DefaultParser) Parse(t ToolUse) (Verdict, bool) {
	// Known-local tools never make outbound HTTP calls; if a placeholder
	// substring appears in their args (a user pasting the placeholder
	// into a chat that gets routed through Skill, an Edit that records
	// a code snippet containing the literal, etc.) the credential isn't
	// being transmitted. Pass through so the inspector doesn't refuse
	// legitimate local work.
	if isLocalOnlyTool(t.Name) {
		return Verdict{
			IsAPICall: false,
			Reason:    "local-only tool (" + t.Name + "); placeholder not transmitted",
		}, true
	}
	if v, ok := parseStructuredFetch(t); ok {
		return v, true
	}
	if v, ok := parseBashCurl(t); ok {
		return v, true
	}
	return Verdict{}, false
}

// localOnlyTools enumerates tool names that never originate outbound
// HTTP traffic. They run inside the agent harness against local state
// (filesystem, in-process planning, sub-skill dispatch). When a
// placeholder substring appears in their args we know the credential
// is not being sent to a third-party service, so the inspector can
// safely pass through instead of routing to the (potentially absent)
// LLM validator and failing closed.
//
// New entries should only be added for tools whose semantics are
// strictly local. Meta-tools that fan out to other tools (Skill,
// Agent) are safe because each sub-tool's tool_use is inspected
// separately when it fires.
var localOnlyTools = map[string]struct{}{
	"Read":          {},
	"Write":         {},
	"Edit":          {},
	"NotebookEdit":  {},
	"Glob":          {},
	"Grep":          {},
	"TodoWrite":     {},
	"BashOutput":    {},
	"KillShell":     {},
	"Skill":         {},
	"Agent":         {},
	"ExitPlanMode":  {},
	"EnterPlanMode": {},
	"EnterWorktree": {},
	"ExitWorktree":  {},
	"ScheduleWakeup": {},
	"ToolSearch":    {},
}

func isLocalOnlyTool(name string) bool {
	return IsLocalOnlyTool(name)
}

// IsLocalOnlyTool reports whether name is in the well-known set of
// tools that never make outbound HTTP calls. Exported so the
// postprocess decision-gate path can short-circuit task-scope
// enforcement for these tools: requiring an approved task scope to
// run Read / Edit / TodoWrite / Skill / etc. is over-restriction
// since they're harness-local and can't exfiltrate credentials.
func IsLocalOnlyTool(name string) bool {
	_, ok := localOnlyTools[name]
	return ok
}

// parseStructuredFetch handles tools whose input is a JSON object with a
// declared `url` field (and optional method/headers). Recognized tool names:
//
//   - WebFetch, web_fetch (Claude Code)
//   - fetch (Cursor, generic)
//   - http_request (custom)
//
// Unknown names with a `url` field still match — we accept any tool that
// declares a top-level URL. The structural test is sound enough that the
// alternative (require a known tool name allowlist) is more brittle than
// helpful.
func parseStructuredFetch(t ToolUse) (Verdict, bool) {
	if len(t.Input) == 0 {
		return Verdict{}, false
	}
	var raw struct {
		URL     string                 `json:"url"`
		Method  string                 `json:"method,omitempty"`
		Headers map[string]any `json:"headers,omitempty"`
		Body    json.RawMessage        `json:"body,omitempty"`
	}
	if err := json.Unmarshal(t.Input, &raw); err != nil {
		return Verdict{}, false
	}
	if raw.URL == "" {
		return Verdict{}, false
	}
	u, err := url.Parse(raw.URL)
	if err != nil || u.Host == "" {
		return Verdict{}, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Verdict{}, false
	}

	creds, placeholders := scanHeadersForShadow(raw.Headers)
	// If no header carries a shadow placeholder but the body or query does,
	// fall through to ambiguous — v1 only handles header credentials at
	// the resolver.
	if len(creds) == 0 {
		return Verdict{
			IsAPICall: false,
			Ambiguous: true,
			Reason:    "structured fetch: placeholder not in known header credential location",
		}, true
	}

	return Verdict{
		IsAPICall:           true,
		Method:              canonicalMethod(raw.Method),
		Host:                u.Hostname(),
		Path:                u.RequestURI(),
		CredentialLocations: creds,
		Placeholders:        placeholders,
		Reason:              "structured fetch with header credential",
	}, true
}

// parseBashCurl recognizes a `Bash` / `shell` cmd whose single leading
// command is curl with a single positional URL argument. Anything more
// complex (subshells, pipes, env interpolation, multiple curls) falls
// through to the validator.
func parseBashCurl(t ToolUse) (Verdict, bool) {
	if len(t.Input) == 0 {
		return Verdict{}, false
	}
	var raw struct {
		Cmd     string `json:"cmd,omitempty"`
		Command string `json:"command,omitempty"`
	}
	if err := json.Unmarshal(t.Input, &raw); err != nil {
		return Verdict{}, false
	}
	cmd := raw.Cmd
	if cmd == "" {
		cmd = raw.Command
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Verdict{}, false
	}

	// Locate the single credentialed simple command in the AST. This
	// permits pipelines (`curl … | jq '.login'`), redirections
	// (`curl … 2>/dev/null > out.json`), and command chains
	// (`set -e && curl …`) as long as exactly one command in the
	// pipeline carries the placeholder and nothing unsafe (command
	// substitution, backticks, process substitution) is present.
	seg, segErr := extractCredentialedCurlSegment(cmd)
	if segErr != "" {
		return Verdict{IsAPICall: false, Ambiguous: true, Reason: segErr}, true
	}
	if seg.text == "" {
		// No credentialed sub-command found. Could be a non-curl call
		// or the placeholder appeared only in a non-curl segment;
		// either way, parser doesn't claim this.
		return Verdict{}, false
	}

	tokens, ok := simpleShellTokenize(seg.text)
	if !ok || len(tokens) == 0 {
		return Verdict{
			IsAPICall: false,
			Ambiguous: true,
			Reason:    "bash: tokenizer rejected input",
		}, true
	}
	if !isCurlInvocation(tokens[0]) {
		return Verdict{}, false
	}

	method := "GET"
	headers := map[string]string{}
	var positionals []string
	i := 1
	for i < len(tokens) {
		tok := tokens[i]
		switch {
		case tok == "-X" || tok == "--request":
			if i+1 >= len(tokens) {
				return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: -X without value"}, true
			}
			method = canonicalMethod(tokens[i+1])
			i += 2
		case tok == "-H" || tok == "--header":
			if i+1 >= len(tokens) {
				return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: -H without value"}, true
			}
			name, value, ok := splitHeader(tokens[i+1])
			if ok {
				headers[name] = value
			}
			i += 2
		case isSafeBoolCurlFlag(tok):
			// Benign no-value flags (`-s`, `--silent`, `-sS`, `--compressed`, …).
			// They don't affect routing or auth, so we can safely accept
			// the call instead of refusing it as ambiguous.
			i++
		case isSafeValueCurlFlag(tok):
			// Value-taking flags that don't affect routing (`-A`, `-o`,
			// `--max-time`, …). Consume the value too.
			if i+1 >= len(tokens) {
				return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: " + tok + " without value"}, true
			}
			i += 2
		case strings.HasPrefix(tok, "-"):
			// Unknown flag — could be -d/--data with a value or a flag we
			// don't safely model. Fall through to validator.
			return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: unknown curl flag " + tok}, true
		default:
			positionals = append(positionals, tok)
			i++
		}
	}
	if len(positionals) != 1 {
		return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: expected exactly one positional URL"}, true
	}

	u, err := url.Parse(positionals[0])
	if err != nil || u.Host == "" {
		return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: positional is not a URL"}, true
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Verdict{IsAPICall: false, Ambiguous: true, Reason: "bash: non-http URL"}, true
	}

	creds, placeholders := scanHeadersForShadow(headersToInterface(headers))
	if len(creds) == 0 {
		return Verdict{
			IsAPICall: false,
			Ambiguous: true,
			Reason:    "bash: placeholder not in -H header",
		}, true
	}

	return Verdict{
		IsAPICall:           true,
		Method:              method,
		Host:                u.Hostname(),
		Path:                u.RequestURI(),
		CredentialLocations: creds,
		Placeholders:        placeholders,
		Reason:              "bash curl with -H credential header",
	}, true
}

// credentialedCurlSegment describes the byte range of the single
// CallExpr inside a (possibly compound) bash command that carries an
// autovault placeholder. The text field is the raw command substring
// inside that range — the rest of the pipeline (e.g. `| jq` after a
// curl) is intentionally outside it.
type credentialedCurlSegment struct {
	text  string
	start int
	end   int
}

// extractCredentialedCurlSegment parses cmd with mvdan/sh and locates
// the single simple-command (CallExpr) whose static text contains an
// autovault placeholder. Pipelines, chains (`&&`/`||`/`;`), and stdout
// redirections are permitted — those constructs operate on the curl's
// OUTPUT, not its credential. Command substitution, process
// substitution, and backticks are refused outright because they let a
// neighboring command exfiltrate the curl's output (which contains
// data the credential authorized).
//
// Returns:
//   - (segment, "") when exactly one credentialed CallExpr is found.
//   - (zero, reason) when something unsafe is present — the caller
//     emits a non-empty Verdict.Reason and refuses.
//   - (zero, "") when no credentialed CallExpr is in the command, so
//     the parser falls through and the validator can inspect.
func extractCredentialedCurlSegment(cmd string) (credentialedCurlSegment, string) {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return credentialedCurlSegment{}, "bash: parse error: " + err.Error()
	}
	if len(file.Stmts) == 0 {
		return credentialedCurlSegment{}, ""
	}
	if len(file.Stmts) > 1 {
		return credentialedCurlSegment{}, "bash: multiple statements; refusing to rewrite"
	}
	stmt := file.Stmts[0]
	if stmt.Negated || stmt.Background || stmt.Coprocess {
		return credentialedCurlSegment{}, "bash: backgrounded/negated statement; refusing to rewrite"
	}

	var (
		callExprs []*syntax.CallExpr
		unsafe    string
	)
	syntax.Walk(file, func(node syntax.Node) bool {
		if unsafe != "" || node == nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CmdSubst:
			unsafe = "bash: command substitution `$(...)` present; refusing to rewrite"
			return false
		case *syntax.ProcSubst:
			unsafe = "bash: process substitution `<(...)` present; refusing to rewrite"
			return false
		case *syntax.CallExpr:
			callExprs = append(callExprs, n)
		}
		return true
	})
	if unsafe != "" {
		return credentialedCurlSegment{}, unsafe
	}
	// Backtick command substitution is parsed by mvdan/sh as a
	// DblQuoted/SglQuoted with a CmdSubst node inside, which the Walk
	// above catches. But a stmt-level redirect whose word carries the
	// placeholder is suspicious — refuse.
	for _, redir := range stmt.Redirs {
		if redir.Word != nil {
			val, ok := staticWordValue(redir.Word)
			if ok && autovault.HeaderMaybeContainsShadow(val) {
				return credentialedCurlSegment{}, "bash: redirect target carries placeholder; refusing"
			}
		}
	}

	var matched []*syntax.CallExpr
	for _, ce := range callExprs {
		if callExprContainsPlaceholder(ce) {
			matched = append(matched, ce)
		}
	}
	if len(matched) == 0 {
		return credentialedCurlSegment{}, ""
	}
	if len(matched) > 1 {
		return credentialedCurlSegment{}, "bash: multiple credentialed commands; refusing to rewrite"
	}
	ce := matched[0]
	start := int(ce.Pos().Offset())
	end := int(ce.End().Offset())
	if start < 0 || end <= start || end > len(cmd) {
		return credentialedCurlSegment{}, "bash: invalid AST positions"
	}
	return credentialedCurlSegment{text: cmd[start:end], start: start, end: end}, ""
}

// callExprContainsPlaceholder reports whether any static-word
// argument inside the call expression contains an autovault
// placeholder substring. Dynamic words (anything that's not
// a literal / quoted string) are conservatively treated as
// not-containing — we err on the side of NOT classifying a
// CallExpr as credentialed in the presence of dynamic args.
func callExprContainsPlaceholder(ce *syntax.CallExpr) bool {
	if ce == nil {
		return false
	}
	for _, word := range ce.Args {
		val, ok := staticWordValue(word)
		if !ok {
			continue
		}
		if autovault.HeaderMaybeContainsShadow(val) {
			return true
		}
	}
	return false
}

// staticWordValue concatenates literal / quoted parts of a Word into
// its text value. Returns (text, true) only when the word is purely
// static (no $var, $(cmd), arithmetic expansion, etc.). Mirrors
// staticShellWord in control.go but lives here so the inspector
// package doesn't take an internal dep on llmproxy.
func staticWordValue(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	return staticWordPartsValue(word.Parts)
}

func staticWordPartsValue(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			value, ok := staticWordPartsValue(p.Parts)
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

// isSafeBoolCurlFlag reports whether tok is a curl flag we know to be
// benign: no value follows, no impact on routing or auth. Returns true
// for both long forms (`--silent`) and short-flag clusters (`-sS`,
// `-fsS`) as long as every short-flag letter is itself benign.
//
// Refused-by-omission: anything that changes URL routing (`-x`/`--proxy`),
// follows redirects (`-L`/`--location`), bypasses TLS (`-k`/`--insecure`),
// loads alternate cert material, or carries a request body (`-d`,
// `--data*`, `-T`, `-F`). Those still fall through to ambiguous so the
// rewriter refuses the call.
func isSafeBoolCurlFlag(tok string) bool {
	if _, ok := safeBoolCurlFlagsExact[tok]; ok {
		return true
	}
	// Short-flag cluster like `-sS` or `-fsS`: each letter must be in
	// the safe single-char set. Two-char `-X` would have matched the
	// switch above; here we're handling 3+ char clusters.
	if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' {
		for _, r := range tok[1:] {
			if _, ok := safeBoolCurlShortFlags[r]; !ok {
				return false
			}
		}
		return true
	}
	return false
}

// isSafeValueCurlFlag reports whether tok is a curl flag that takes
// exactly one value but does not affect routing or auth.
func isSafeValueCurlFlag(tok string) bool {
	_, ok := safeValueCurlFlagsExact[tok]
	return ok
}

// safeBoolCurlFlagsExact lists boolean flags accepted verbatim.
var safeBoolCurlFlagsExact = map[string]struct{}{
	"-s":               {},
	"-S":               {},
	"--silent":         {},
	"--show-error":     {},
	"-f":               {},
	"--fail":           {},
	"--fail-with-body": {},
	"-i":               {},
	"--include":        {},
	"--compressed":     {},
	"-#":               {},
	"--progress-bar":   {},
	"-v":               {},
	"--verbose":        {},
	"-G":               {},
	"--get":            {},
	"-J":               {},
	"--remote-header-name": {},
	"-O":               {},
	"--remote-name":    {},
	"-N":               {},
	"--no-buffer":      {},
	"-4":               {},
	"-6":               {},
	"--ipv4":           {},
	"--ipv6":           {},
}

// safeBoolCurlShortFlags is the set of single-character boolean flags
// allowed inside a short-flag cluster like `-sS` / `-fsS`.
var safeBoolCurlShortFlags = map[rune]struct{}{
	's': {}, 'S': {}, 'f': {}, 'i': {}, 'v': {}, 'G': {}, 'J': {}, 'N': {}, '4': {}, '6': {},
}

// safeValueCurlFlagsExact lists flags that consume a single following
// value and do not affect routing or auth.
var safeValueCurlFlagsExact = map[string]struct{}{
	"-A":                {},
	"--user-agent":      {},
	"-e":                {},
	"--referer":         {},
	"-o":                {},
	"--output":          {},
	"-w":                {},
	"--write-out":       {},
	"-m":                {},
	"--max-time":        {},
	"--connect-timeout": {},
	"--retry":           {},
	"--retry-delay":     {},
	"--retry-max-time": {},
	"--max-redirs":      {},
	"--resolve":         {},
}

// scanHeadersForShadow returns the credential locations and the actual
// placeholder strings found in headers where a shadow placeholder
// appears. Keys are normalized to canonical MIME-Header-Case.
//
// Returning the placeholder values lets the downstream boundary check
// look up the placeholder's bound service without re-parsing.
func scanHeadersForShadow(headers map[string]any) ([]CredentialLocation, []string) {
	if len(headers) == 0 {
		return nil, nil
	}
	var locs []CredentialLocation
	var placeholders []string
	for name, raw := range headers {
		value, ok := raw.(string)
		if !ok {
			continue
		}
		if !autovault.HeaderMaybeContainsShadow(value) {
			continue
		}
		scheme := ""
		if idx := strings.IndexByte(value, ' '); idx > 0 {
			s := strings.ToLower(value[:idx])
			switch s {
			case "bearer":
				scheme = "Bearer"
			case "basic":
				scheme = "Basic"
			case "token":
				scheme = "Token"
			}
		}
		locs = append(locs, CredentialLocation{
			Kind:   "header",
			Name:   canonicalHeaderName(name),
			Scheme: scheme,
		})
		// Extract the placeholder substring from the header value. For
		// `Bearer autovault_github_xyz`, that's `autovault_github_xyz`.
		// For Basic auth (base64-encoded user:pass) we'd need to decode,
		// which `HeaderMaybeContainsShadow` already does as a check —
		// for v1 we conservatively don't extract the placeholder from
		// Basic auth headers.
		for _, candidate := range autovaultPlaceholderRE.FindAllString(value, -1) {
			if autovault.LooksLikeShadow(candidate) {
				placeholders = append(placeholders, candidate)
			}
		}
	}
	return locs, placeholders
}

// autovaultPlaceholderRE pulls placeholder tokens out of a header value
// without false-matching log-line / comment context that may share part
// of the substring. Mirror of the runtime-proxy's autovault swap regex.
var autovaultPlaceholderRE = regexp.MustCompile(`[A-Za-z0-9._:-]*autovault[A-Za-z0-9._:-]+`)

func canonicalHeaderName(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

func headersToInterface(in map[string]string) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func splitHeader(raw string) (name, value string, ok bool) {
	idx := strings.IndexByte(raw, ':')
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(raw[:idx]), strings.TrimSpace(raw[idx+1:]), true
}

// hasShellMetacharacter is a coarse pre-filter. Anything matching is out of
// scope per the design's Bash envelope and gets refused. The deny-list lives
// here (not just at the rewriter) so we don't accidentally classify a
// shell-injection-shaped input as a clean curl.
//
// Quote-aware: characters that appear inside a single- or double-quoted
// region of the command line are literal, not shell metacharacters. Without
// this, valid curls like
//
//	curl 'https://api.github.com/repos/x/y/issues?state=open&labels=bug'
//
// would be refused because of the `&` inside the URL's query string.
// Backtick is the lone exception — it's still treated as metacharacter
// inside double quotes since bash performs command substitution there.
func hasShellMetacharacter(cmd string) bool {
	var state rune // 0, '\'', '"'
	for _, c := range cmd {
		switch {
		case state == 0 && (c == '\'' || c == '"'):
			state = c
		case state != 0 && c == state:
			state = 0
		case state == '\'':
			// Inside single quotes: every char is literal.
		case state == '"':
			// Inside double quotes: $ and ` still trigger substitution.
			if c == '$' || c == '`' {
				return true
			}
		default:
			// Unquoted.
			switch c {
			case '|', ';', '&', '`', '$', '<', '>', '(', ')', '{', '}':
				return true
			}
		}
	}
	// Catch backslash newlines specifically (multi-line via line continuation
	// is out of scope for v1).
	if strings.Contains(cmd, "\\\n") {
		return true
	}
	return false
}

// simpleShellTokenize is a minimal tokenizer: splits on whitespace,
// respecting single/double quotes. Returns false if quotes are unbalanced.
//
// This is intentionally a small, auditable function rather than a heavy
// dependency. The Bash envelope is intentionally narrow; mvdan/sh can be
// swapped in later if/when that envelope widens.
func simpleShellTokenize(cmd string) ([]string, bool) {
	var (
		tokens []string
		buf    strings.Builder
		state  rune // 0, '\'', '"'
	)
	flush := func() {
		if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
	}
	// Iterate runes, not bytes — rune(cmd[i]) treats a single byte as a
	// codepoint, which corrupts multi-byte UTF-8 (e.g. é → two bogus
	// runes 0xC3, 0xA9 each WriteRune-encoded back to UTF-8 separately,
	// turning one é into four bytes of garbage).
	for _, c := range cmd {
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

func isCurlInvocation(token string) bool {
	switch strings.ToLower(token) {
	case "curl", "/usr/bin/curl", "/bin/curl":
		return true
	}
	return false
}
