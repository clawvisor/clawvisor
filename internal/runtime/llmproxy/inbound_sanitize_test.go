package llmproxy

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// The post-rewrite shape — what the harness re-sends back to /v1/messages
// after one successful tool turn — must NEVER reach the model on the
// next inbound. SanitizeInboundHistory reverts URLs to the synthetic
// form and drops the proxy-injected headers.
func TestSanitizeInboundHistory_AnthropicRevertsRewrittenCurl(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4",
		"messages": [
			{"role": "user", "content": "fetch /user"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "ok"},
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {
					"command": "curl -sS -H 'Authorization: Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' -H 'X-Clawvisor-Target-Host: api.github.com' -H 'X-Clawvisor-Caller: Bearer cv-nonce-abc123' http://localhost:25297/api/proxy/user"
				}}
			]}
		]
	}`)
	res, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if !res.Modified {
		t.Fatalf("expected Modified=true; sanitizer was a no-op")
	}
	out := string(res.Body)

	// Must contain the original synthetic shape the model would have
	// emitted.
	if !strings.Contains(out, "https://api.github.com/user") {
		t.Errorf("reverted URL missing in: %s", out)
	}
	if !strings.Contains(out, "Authorization: Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") {
		t.Errorf("placeholder Authorization header must survive sanitization: %s", out)
	}

	// Must NOT contain the post-rewrite transport details.
	for _, banned := range []string{
		"cv-nonce-abc123",
		"X-Clawvisor-Caller",
		"X-Clawvisor-Target-Host",
		"http://localhost:25297",
		"/api/proxy/user",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("post-rewrite leak: %q still present in sanitized body: %s", banned, out)
		}
	}
}

// Control-plane rewrites have a different shape: the rewriter targets
// /api/control/... and uses clawvisor.local as the target host. The
// reversion goes to the synthetic clawvisor.local URL.
func TestSanitizeInboundHistory_AnthropicRevertsControlCurl(t *testing.T) {
	body := []byte(`{
		"messages": [{"role": "assistant", "content": [
			{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {
				"command": "curl -sS -X POST -H 'X-Clawvisor-Target-Host: clawvisor.local' -H 'X-Clawvisor-Caller: Bearer cv-nonce-xyz' http://localhost:25297/api/control/tasks --data '{}'"
			}}
		]}]
	}`)
	res, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if !res.Modified {
		t.Fatalf("expected Modified=true")
	}
	out := string(res.Body)
	if !strings.Contains(out, "https://clawvisor.local/control/tasks") {
		t.Errorf("control URL not reverted: %s", out)
	}
	if strings.Contains(out, "cv-nonce-") || strings.Contains(out, "X-Clawvisor-") {
		t.Errorf("control sanitization left leaks in: %s", out)
	}
}

// Cheap pre-filter: a body without any rewrite calling-cards must
// short-circuit and return Modified=false. (No JSON re-encoding cost.)
func TestSanitizeInboundHistory_NoOpWhenNothingToStrip(t *testing.T) {
	body := []byte(`{"messages": [{"role":"user","content":"hi"}]}`)
	res, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if res.Modified {
		t.Errorf("expected no-op on clean body, got Modified=true")
	}
	if string(res.Body) != string(body) {
		t.Errorf("clean body should pass through verbatim")
	}
}

// User-role messages must never be touched, even if they happen to
// contain rewrite-shaped substrings (e.g. the user pasted a log line).
func TestSanitizeInboundHistory_LeavesUserMessagesAlone(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "saw cv-nonce-abc in the log"},
			{"role": "assistant", "content": "ok"}
		]
	}`)
	res, _ := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	// The pre-filter sees "cv-nonce-" and engages, but the walk only
	// touches assistant tool_use blocks. No tool_use → no mutation.
	if res.Modified {
		t.Errorf("user-role mention must not be modified")
	}
	if !strings.Contains(string(res.Body), "saw cv-nonce-abc in the log") {
		t.Errorf("user content must survive: %s", res.Body)
	}
}

// OpenAI Chat Completions stores the bash command as a JSON-encoded
// string inside tool_calls[].function.arguments. The sanitizer must
// re-encode after mutation.
func TestSanitizeInboundHistory_OpenAIChatRevertsArguments(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"assistant","tool_calls":[
			{"id":"call_1","type":"function","function":{
				"name":"Bash",
				"arguments":"{\"command\":\"curl -sS -H 'Authorization: Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' -H 'X-Clawvisor-Target-Host: api.github.com' -H 'X-Clawvisor-Caller: Bearer cv-nonce-abc' http://localhost:25297/api/proxy/user\"}"
			}}
		]}]
	}`)
	res, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderOpenAI,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if !res.Modified {
		t.Fatalf("expected OpenAI Chat sanitize to fire")
	}
	out := string(res.Body)
	if !strings.Contains(out, "https://api.github.com/user") {
		t.Errorf("OpenAI: URL not reverted: %s", out)
	}
	if strings.Contains(out, "cv-nonce-") || strings.Contains(out, "X-Clawvisor-") {
		t.Errorf("OpenAI sanitization missed a leak: %s", out)
	}
}

// Regression: only assistant-role messages may carry rewriter
// transport details legitimately. A non-assistant message with a
// tool_calls field is at best malformed; the sanitizer must not
// mutate it.
func TestSanitizeInboundHistory_OpenAINonAssistantToolCallsUnchanged(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"tool","tool_call_id":"call_1","tool_calls":[
			{"id":"call_1","type":"function","function":{
				"name":"Bash",
				"arguments":"{\"command\":\"curl -H 'X-Clawvisor-Caller: Bearer cv-nonce-stale' http://localhost:25297/api/proxy/x\"}"
			}}
		]}]
	}`)
	res, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderOpenAI,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if res.Modified {
		t.Errorf("non-assistant tool_calls must not be sanitized")
	}
	// The post-rewrite contents must still be present (the test
	// would have failed even with a no-op if we accidentally
	// mutated). Defensively assert the cv-nonce token survives.
	if !strings.Contains(string(res.Body), "cv-nonce-stale") {
		t.Errorf("non-assistant payload was unexpectedly stripped: %s", res.Body)
	}
}

// Defensive: the sanitizer must be idempotent. Running it on an
// already-sanitized body must produce the same bytes back.
func TestSanitizeInboundHistory_IsIdempotent(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_1","name":"Bash","input":{
				"command":"curl -sS -H 'X-Clawvisor-Target-Host: api.github.com' -H 'X-Clawvisor-Caller: Bearer cv-nonce-1' http://localhost:25297/api/proxy/user"
			}}
		]}]
	}`)
	req := SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	}
	first, _ := SanitizeInboundHistory(req)
	if !first.Modified {
		t.Fatalf("first pass must modify")
	}
	req.Body = first.Body
	second, _ := SanitizeInboundHistory(req)
	if second.Modified {
		t.Errorf("second pass must be a no-op; got modified=true\nfirst=%s\nsecond=%s",
			first.Body, second.Body)
	}
}

// A model that pasted `cv-nonce-…` into an unrelated field (e.g. a
// description) must have it redacted to a placeholder marker.
func TestSanitizeBashCommand_RedactsLooseNonces(t *testing.T) {
	got, mut := sanitizeBashCommand("echo 'cv-nonce-leaked-into-comment'", SanitizeInboundRequest{
		ResolverBaseURL: "http://localhost:25297/api/proxy",
		ControlBaseURL:  "http://localhost:25297",
	})
	if !mut {
		t.Fatalf("loose nonce must be sanitized")
	}
	if strings.Contains(got, "cv-nonce-leaked-into-comment") {
		t.Errorf("loose nonce survived: %q", got)
	}
	if !strings.Contains(got, "[clawvisor-managed]") {
		t.Errorf("expected redaction marker, got %q", got)
	}
}

// TestBuildSyntheticURL_RejectsPathSmugglingHosts is a regression test
// for an attacker-influenced X-Clawvisor-Target-Host that contains URL
// path metacharacters. Without strict host validation the rebuilt
// inbound-history URL would include the metacharacters verbatim.
func TestBuildSyntheticURL_RejectsPathSmugglingHosts(t *testing.T) {
	cases := []struct {
		host string
		want bool // true => buildSyntheticURL should accept
	}{
		{"api.example.com", true},
		{"api.example.com:8443", true},
		{"api-v2.example_internal.com", true},
		{"[2001:db8::1]:8443", true},
		{"[::1]", true},
		// Path-smuggling shapes:
		{"evil.com/../legit.com", false},
		{"evil.com?legit=true", false},
		{"evil.com#frag", false},
		{"evil.com@phish.example", false},
		{"evil.com%2flegit.com", false},
		{"evil.com\\backslash", false},
		// Whitespace was already rejected, but exercise it.
		{"evil.com /legit.com", false},
		// Port nonsense.
		{"evil.com:abc", false},
		{"evil.com:99999", true}, // 5 chars, narrow accept; resolver still validates downstream
		{"evil.com:999999", false},
	}
	for _, c := range cases {
		got := buildSyntheticURL(c.host, "/x")
		accepted := got != ""
		if accepted != c.want {
			t.Errorf("buildSyntheticURL(%q): accepted=%v want=%v (got %q)", c.host, accepted, c.want, got)
		}
	}
}
