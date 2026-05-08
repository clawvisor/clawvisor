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
	// also be a single token.
	for i, tok := range tokens {
		if tok == "-H" && i+1 < len(tokens) && strings.HasPrefix(tokens[i+1], "X-Clawvisor-Caller:") {
			if tokens[i+1] != "X-Clawvisor-Caller: Bearer cvis_call" {
				t.Fatalf("X-Clawvisor-Caller value mangled: %q", tokens[i+1])
			}
		}
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
