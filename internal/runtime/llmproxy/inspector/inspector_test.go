package inspector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func toolUse(name, input string) ToolUse {
	return ToolUse{ID: "toolu_1", Name: name, Input: json.RawMessage(input)}
}

func TestTriggerHits(t *testing.T) {
	cases := []struct {
		name string
		in   ToolUse
		want bool
	}{
		{"empty", toolUse("Bash", ""), false},
		{"no shadow", toolUse("Bash", `{"cmd":"ls"}`), false},
		{"autovault", toolUse("Bash", `{"cmd":"curl -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/repos/x/y"}`), true},
		{"clawvisor legacy", toolUse("Bash", `{"cmd":"echo clawvisor_x"}`), true},
		{"unrelated autovault word", toolUse("Bash", `{"cmd":"echo autovaults"}`), false},
		{"clawvisor repo path", toolUse("exec_command", `{"cmd":"pwd","workdir":"/Users/ericlevine/conductor/workspaces/clawvisor-public/san-francisco-v5"}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TriggerHits(tc.in); got != tc.want {
				t.Fatalf("TriggerHits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultParser_StructuredFetch(t *testing.T) {
	p := DefaultParser{}
	in := toolUse("WebFetch", `{
		"url":"https://api.github.com/repos/x/y/issues",
		"method":"POST",
		"headers":{"Authorization":"Bearer autovault_github_abc"}
	}`)
	v, ok := p.Parse(in)
	if !ok {
		t.Fatalf("expected parser match")
	}
	if !v.IsAPICall {
		t.Fatalf("expected IsAPICall true, got %+v", v)
	}
	if v.Host != "api.github.com" {
		t.Fatalf("expected host api.github.com, got %q", v.Host)
	}
	if v.Method != "POST" {
		t.Fatalf("expected POST, got %q", v.Method)
	}
	if len(v.CredentialLocations) != 1 || v.CredentialLocations[0].Name != "Authorization" {
		t.Fatalf("expected one Authorization credential location, got %+v", v.CredentialLocations)
	}
}

func TestDefaultParser_BashCleanCurl(t *testing.T) {
	p := DefaultParser{}
	in := toolUse("Bash", `{"cmd":"curl -X POST -H 'Authorization: Bearer autovault_github_abc' https://api.github.com/repos/x/y/issues"}`)
	v, ok := p.Parse(in)
	if !ok {
		t.Fatalf("expected parser match")
	}
	if !v.IsAPICall {
		t.Fatalf("expected IsAPICall true, got %+v", v)
	}
	if v.Host != "api.github.com" {
		t.Fatalf("expected host api.github.com, got %q", v.Host)
	}
	if v.Method != "POST" {
		t.Fatalf("expected POST, got %q", v.Method)
	}
}

func TestDefaultParser_BashShellMetacharacterRefused(t *testing.T) {
	p := DefaultParser{}
	in := toolUse("Bash", `{"cmd":"echo autovault_github_xxx | tee /tmp/leak"}`)
	v, ok := p.Parse(in)
	if !ok {
		t.Fatalf("expected parser to consume input (return ambiguous)")
	}
	if !v.Ambiguous {
		t.Fatalf("expected ambiguous on shell metacharacters, got %+v", v)
	}
}

func TestDefaultParser_FallthroughOnUnknown(t *testing.T) {
	p := DefaultParser{}
	in := toolUse("CustomTool", `{"foo":"autovault_github_xxx"}`)
	if _, ok := p.Parse(in); ok {
		t.Fatalf("expected fallthrough for unknown tool shape")
	}
}

// hasShellMetacharacter must be quote-aware: characters that appear
// inside single- or double-quoted regions of a curl line are literal,
// not shell ops. Without this, real-world URLs containing & in their
// query string are wrongly classified as injection-shaped and refused.
func TestHasShellMetacharacter_QuoteAware(t *testing.T) {
	safe := []string{
		`curl 'https://api.github.com/repos/x/y/issues?state=open&labels=bug'`,
		`curl "https://api.github.com/x?a=1&b=2"`,
		`curl -H 'Accept: application/json; charset=utf-8' https://api.github.com/x`,
		`curl 'https://example.com/?q=foo|bar'`, // pipe inside quotes
	}
	for _, s := range safe {
		if hasShellMetacharacter(s) {
			t.Errorf("safe quoted input rejected: %q", s)
		}
	}
	dangerous := []string{
		`curl https://api.github.com/x ; rm -rf /`,                  // unquoted ;
		`curl https://api.github.com/x | cat`,                       // unquoted pipe
		`curl $(whoami).github.com`,                                 // command substitution
		`curl "https://api.github.com/$(whoami)"`,                   // $ inside double quotes
		"curl \"https://example.com/`whoami`\"",                     // backtick inside double quotes
		`curl https://api.github.com/x && rm`,                       // unquoted &&
	}
	for _, s := range dangerous {
		if !hasShellMetacharacter(s) {
			t.Errorf("dangerous input accepted: %q", s)
		}
	}
}

func TestInspector_TriggerMissShortCircuits(t *testing.T) {
	insp := NewInspector(DefaultParser{}, AmbiguousValidator{})
	v := insp.Inspect(context.Background(), toolUse("Bash", `{"cmd":"ls"}`))
	if v.Source != SourceTriggerMiss {
		t.Fatalf("expected trigger_miss, got %s", v.Source)
	}
	if v.IsAPICall {
		t.Fatalf("trigger miss should never be IsAPICall")
	}
}

func TestInspector_FallsThroughToValidator(t *testing.T) {
	insp := NewInspector(DefaultParser{}, AmbiguousValidator{})
	in := toolUse("CustomTool", `{"foo":"autovault_github_xxx"}`)
	v := insp.Inspect(context.Background(), in)
	if v.Source != SourceValidator {
		t.Fatalf("expected validator source, got %s", v.Source)
	}
	if !v.Ambiguous {
		t.Fatalf("AmbiguousValidator should produce ambiguous=true")
	}
}

func TestRewrite_StructuredFetch(t *testing.T) {
	in := toolUse("WebFetch", `{
		"url":"https://api.github.com/repos/x/y/issues?state=open",
		"method":"POST",
		"headers":{"Authorization":"Bearer autovault_github_abc"},
		"body":"{}"
	}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup: parser did not classify as IsAPICall")
	}
	out, err := Rewrite(in, v, DefaultRewriteOpts("https://proxy.clawvisor.example/proxy/v1"))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten output not JSON: %v", err)
	}
	url, _ := got["url"].(string)
	if !strings.HasPrefix(url, "https://proxy.clawvisor.example/proxy/v1/repos/x/y/issues") {
		t.Fatalf("rewritten url unexpected: %q", url)
	}
	if !strings.Contains(url, "state=open") {
		t.Fatalf("query string lost on rewrite: %q", url)
	}
	headers, _ := got["headers"].(map[string]any)
	if headers["X-Clawvisor-Target-Host"] != "api.github.com" {
		t.Fatalf("X-Clawvisor-Target-Host missing or wrong: %+v", headers)
	}
	if headers["Authorization"] != "Bearer autovault_github_abc" {
		t.Fatalf("Authorization placeholder lost: %+v", headers)
	}
}

func TestRewrite_BashAddsTargetHostHeader(t *testing.T) {
	in := toolUse("Bash", `{"cmd":"curl -X POST -H 'Authorization: Bearer autovault_github_abc' https://api.github.com/repos/x/y/issues"}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup: parser did not classify as IsAPICall")
	}
	out, err := Rewrite(in, v, DefaultRewriteOpts("https://proxy.clawvisor.example/proxy/v1"))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten output not JSON: %v", err)
	}
	cmd, _ := got["cmd"].(string)
	if !strings.Contains(cmd, "https://proxy.clawvisor.example/proxy/v1/repos/x/y/issues") {
		t.Fatalf("rewritten cmd missing resolver URL: %q", cmd)
	}
	if !strings.Contains(cmd, "X-Clawvisor-Target-Host: api.github.com") {
		t.Fatalf("rewritten cmd missing target-host header: %q", cmd)
	}
	if !strings.Contains(cmd, "Authorization: Bearer autovault_github_abc") {
		t.Fatalf("Authorization placeholder lost: %q", cmd)
	}
}

// An explicit non-default port on the original URL must survive the
// rewriter round-trip via the X-Clawvisor-Target-Host header. Without
// this, a URL like https://api.github.com:8443/... would be forwarded
// to the default port (443) at the resolver.
func TestRewrite_BashPreservesExplicitPort(t *testing.T) {
	in := toolUse("Bash", `{"cmd":"curl -X POST -H 'Authorization: Bearer autovault_github_abc' https://api.github.com:8443/repos/x/y/issues"}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup: parser did not classify as IsAPICall")
	}
	out, err := Rewrite(in, v, DefaultRewriteOpts("https://proxy.clawvisor.example/proxy/v1"))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten output not JSON: %v", err)
	}
	cmd, _ := got["cmd"].(string)
	if !strings.Contains(cmd, "X-Clawvisor-Target-Host: api.github.com:8443") {
		t.Fatalf("explicit port dropped from target-host header: %q", cmd)
	}
}

// Structured rewrites already include the port via url.URL.Host; this
// test pins the behavior so a future change can't regress it without
// flipping this assertion.
func TestRewrite_StructuredPreservesExplicitPort(t *testing.T) {
	in := toolUse("WebFetch", `{
		"url":"https://api.github.com:8443/repos/x/y/issues",
		"method":"POST",
		"headers":{"Authorization":"Bearer autovault_github_abc"}
	}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup: parser did not classify as IsAPICall")
	}
	out, err := Rewrite(in, v, DefaultRewriteOpts("https://proxy.clawvisor.example/proxy/v1"))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten output not JSON: %v", err)
	}
	headers, _ := got["headers"].(map[string]any)
	if headers == nil {
		t.Fatalf("rewritten output missing headers: %s", out)
	}
	if headers["X-Clawvisor-Target-Host"] != "api.github.com:8443" {
		t.Fatalf("explicit port dropped from target-host header: %v", headers["X-Clawvisor-Target-Host"])
	}
}

// Security: a deny-list of shell metacharacters misses things. The
// allow-list in quoteShell must quote any token containing a newline,
// tab, glob char, brace, bracket, or comment marker — otherwise model-
// generated tool input could inject commands via the rebuilt shell line.
func TestRewrite_BashQuotesDangerousTokens(t *testing.T) {
	// Each test case feeds the curl line a header value containing a
	// dangerous character. The rewriter must produce a command that
	// keeps the value inside a single argument.
	cases := []struct {
		name      string
		headerVal string
	}{
		{"newline_injection", "Bearer autovault_x\nrm -rf /"},
		{"tab", "Bearer autovault_x\tinjected"},
		{"comment_marker", "Bearer autovault_x #ignored"},
		{"glob_star", "Bearer autovault_x*"},
		{"glob_question", "Bearer autovault_x?"},
		{"brace_expand", "Bearer autovault_x{a,b}"},
		{"bracket_expand", "Bearer autovault_x[abc]"},
		{"home_tilde", "Bearer autovault_x~"},
		{"backtick", "Bearer autovault_x`whoami`"},
		{"dollar", "Bearer autovault_x$SHELL"},
		{"semicolon", "Bearer autovault_x;rm"},
		{"pipe", "Bearer autovault_x|cat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := quoteShell(tc.headerVal)
			// The quoted result must start with a single-quote (we use
			// single-quote wrapping for any non-safe token).
			if !strings.HasPrefix(out, "'") || !strings.HasSuffix(out, "'") {
				t.Fatalf("dangerous token %q not quoted: %q", tc.headerVal, out)
			}
		})
	}
}

func TestRewrite_BashLeavesSafeTokensUnquoted(t *testing.T) {
	// Tokens composed of only the safe allow-list pass through verbatim
	// (no spurious quoting that would corrupt the command line).
	cases := []string{
		"curl",
		"-X",
		"POST",
		"https://api.github.com/repos/x/y",
		"Authorization:",
		"abc123-_.+@example",
	}
	for _, s := range cases {
		if out := quoteShell(s); out != s {
			t.Errorf("safe token %q was unnecessarily quoted as %q", s, out)
		}
	}
}

func TestRewrite_AmbiguousReturnsErr(t *testing.T) {
	v := Verdict{Ambiguous: true}
	if _, err := Rewrite(ToolUse{}, v, DefaultRewriteOpts("https://proxy")); err != ErrAmbiguous {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
}

func TestRewrite_InjectsCallerToken_Structured(t *testing.T) {
	in := toolUse("WebFetch", `{
		"url":"https://api.github.com/repos/x/y/issues",
		"method":"POST",
		"headers":{"Authorization":"Bearer autovault_github_xxx"}
	}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup")
	}
	opts := DefaultRewriteOpts("https://proxy.example/proxy/v1")
	opts.CallerToken = "cvis_abc123"

	out, err := Rewrite(in, v, opts)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten not JSON: %v", err)
	}
	headers, _ := got["headers"].(map[string]any)
	if headers["X-Clawvisor-Caller"] != "Bearer cvis_abc123" {
		t.Fatalf("expected X-Clawvisor-Caller=Bearer cvis_abc123, got %+v", headers)
	}
}

func TestRewrite_InjectsCallerToken_Bash(t *testing.T) {
	in := toolUse("Bash", `{"cmd":"curl -X POST -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/repos/x/y/issues"}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup")
	}
	opts := DefaultRewriteOpts("https://proxy.example/proxy/v1")
	opts.CallerToken = "cvis_bash_token"

	out, err := Rewrite(in, v, opts)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten not JSON: %v", err)
	}
	cmd, _ := got["cmd"].(string)
	if !strings.Contains(cmd, "X-Clawvisor-Caller: Bearer cvis_bash_token") {
		t.Fatalf("rewritten cmd missing caller header: %q", cmd)
	}
}

// Regression: simpleShellTokenize strips quotes; the rejoin must
// re-quote tokens that contain whitespace so a value like
// `Authorization: Bearer autovault_xxx` survives intact instead of being
// split into three positionals (`-H Authorization: Bearer autovault_xxx`)
// and lost as far as the harness shell is concerned.
func TestRewrite_BashPreservesQuotedHeaderValue(t *testing.T) {
	in := toolUse("Bash", `{"cmd":"curl -X POST -H 'Authorization: Bearer autovault_github_xyz' https://api.github.com/repos/x/y/issues"}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok || !v.IsAPICall {
		t.Fatalf("setup")
	}
	opts := DefaultRewriteOpts("https://proxy.example/proxy/v1")
	opts.CallerToken = "cvis_call"

	out, err := Rewrite(in, v, opts)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten not JSON: %v", err)
	}
	cmd, _ := got["cmd"].(string)

	// Re-tokenize the rewritten cmd; the original Authorization header
	// value must come back as a SINGLE token, not three.
	tokens, ok := simpleShellTokenize(cmd)
	if !ok {
		t.Fatalf("rewritten cmd doesn't tokenize: %q", cmd)
	}
	found := false
	for i, tok := range tokens {
		if tok == "-H" && i+1 < len(tokens) && strings.HasPrefix(tokens[i+1], "Authorization:") {
			if tokens[i+1] != "Authorization: Bearer autovault_github_xyz" {
				t.Fatalf("Authorization -H value mangled: %q", tokens[i+1])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("Authorization -H not preserved as single token: %v", tokens)
	}

	// Sanity: the injected X-Clawvisor-Caller value with a space must
	// also be a single token. Assert we found it AND that it's intact;
	// without the foundCaller flag, an entirely-missing header would
	// silently pass.
	foundCaller := false
	for i, tok := range tokens {
		if tok == "-H" && i+1 < len(tokens) && strings.HasPrefix(tokens[i+1], "X-Clawvisor-Caller:") {
			foundCaller = true
			if tokens[i+1] != "X-Clawvisor-Caller: Bearer cvis_call" {
				t.Fatalf("X-Clawvisor-Caller value mangled: %q", tokens[i+1])
			}
		}
	}
	if !foundCaller {
		t.Fatalf("X-Clawvisor-Caller header not injected at all: tokens=%v", tokens)
	}
}

func TestVerdict_PlaceholdersExtracted(t *testing.T) {
	in := toolUse("WebFetch", `{
		"url":"https://api.github.com/x",
		"headers":{"Authorization":"Bearer autovault_github_realtoken"}
	}`)
	v, ok := DefaultParser{}.Parse(in)
	if !ok {
		t.Fatalf("expected parser match")
	}
	if len(v.Placeholders) != 1 || v.Placeholders[0] != "autovault_github_realtoken" {
		t.Fatalf("expected one placeholder extracted, got %+v", v.Placeholders)
	}
}

// validator path: the LLM-backed validator intentionally doesn't return
// Placeholders (we don't trust the model to enumerate them). The inspector
// must extract them from the raw input bytes after the validator runs,
// otherwise BoundaryCheck fails closed on empty Placeholders and the
// entire validator fallback is dead code for any Allow decision.
type fakeValidator struct {
	verdict Verdict
}

func (f fakeValidator) Validate(_ context.Context, _ ToolUse) (Verdict, error) {
	return f.verdict, nil
}

func TestInspector_ValidatorPathExtractsPlaceholdersFromInput(t *testing.T) {
	// Parser refuses; falls through to validator.
	insp := NewInspector(refusingParser{}, fakeValidator{verdict: Verdict{
		IsAPICall: true,
		Method:    "POST",
		Host:      "api.github.com",
		Path:      "/repos/x/y/issues",
	}})
	in := toolUse("WebFetch", `{
		"url":"https://api.github.com/repos/x/y/issues",
		"headers":{"Authorization":"Bearer autovault_github_abc123"}
	}`)
	v := insp.Inspect(context.Background(), in)
	if v.Source != SourceValidator {
		t.Fatalf("expected validator source, got %q", v.Source)
	}
	if len(v.Placeholders) != 1 || v.Placeholders[0] != "autovault_github_abc123" {
		t.Fatalf("validator path failed to extract placeholders from input: %+v", v.Placeholders)
	}
}

func TestInspector_ValidatorPathDedupesMultiplePlaceholderMatches(t *testing.T) {
	insp := NewInspector(refusingParser{}, fakeValidator{verdict: Verdict{IsAPICall: true}})
	in := toolUse("custom", `{
		"a":"Bearer autovault_github_xyz",
		"b":"autovault_github_xyz again",
		"c":"autovault_github_other"
	}`)
	v := insp.Inspect(context.Background(), in)
	want := map[string]bool{"autovault_github_xyz": true, "autovault_github_other": true}
	if len(v.Placeholders) != 2 {
		t.Fatalf("expected 2 distinct placeholders, got %+v", v.Placeholders)
	}
	// Track which expected placeholders we've actually seen — without
	// this, [autovault_github_xyz, autovault_github_xyz] (dedup broken)
	// would satisfy len==2 and every value being in want.
	seen := make(map[string]bool, len(want))
	for _, p := range v.Placeholders {
		if !want[p] {
			t.Fatalf("unexpected placeholder: %q", p)
		}
		if seen[p] {
			t.Fatalf("placeholder %q appeared more than once — dedup broken: %+v", p, v.Placeholders)
		}
		seen[p] = true
	}
	for p := range want {
		if !seen[p] {
			t.Fatalf("expected placeholder %q not present in result: %+v", p, v.Placeholders)
		}
	}
}

// refusingParser never matches; forces the inspector into the validator path.
type refusingParser struct{}

func (refusingParser) Parse(_ ToolUse) (Verdict, bool) { return Verdict{}, false }

func TestBoundaryCheck(t *testing.T) {
	allowed := []string{"api.github.com", "*.github.com"}
	cases := []struct {
		name string
		v    Verdict
		ok   bool
	}{
		{"exact", Verdict{IsAPICall: true, Host: "api.github.com"}, true},
		{"suffix wildcard", Verdict{IsAPICall: true, Host: "uploads.github.com"}, true},
		{"non-matching domain suffix", Verdict{IsAPICall: true, Host: "api.github.com.attacker.com"}, false},
		{"ambiguous fails closed", Verdict{IsAPICall: true, Ambiguous: true, Host: "api.github.com"}, false},
		{"missing host", Verdict{IsAPICall: true, Host: ""}, false},
		{"unknown host", Verdict{IsAPICall: true, Host: "evil.example"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, _ := BoundaryCheck(tc.v, allowed)
			if ok != tc.ok {
				t.Fatalf("BoundaryCheck = %v, want %v", ok, tc.ok)
			}
		})
	}
}
