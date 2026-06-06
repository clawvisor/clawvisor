package policies_test

import (
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestInboundSanitize_AllowWithoutMutation pins the no-op path:
// a body with no proxy-rewritten artifacts passes through unchanged.
func TestInboundSanitize_AllowWithoutMutation(t *testing.T) {
	p := policies.NewInboundSanitize("http://localhost:25297/api/proxy", "http://localhost:25297")
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutation on clean body, got %d ReplaceBody calls", len(mut.ReplaceBodyCalls))
	}
}

// TestInboundSanitize_StripsRewrittenToolUseInputs verifies the
// migration path: a body whose assistant history carries a tool_use
// with a proxy-rewritten curl (cv-nonce + X-Clawvisor-Caller header
// + localhost URL) triggers ReplaceBody and the audit flag.
func TestInboundSanitize_StripsRewrittenToolUseInputs(t *testing.T) {
	// Anthropic assistant turn carrying a Bash tool_use whose `cmd`
	// argument has the proxy's transport details. The sanitize helper
	// recognizes the rewritten shape and reverts it to the synthetic
	// host form.
	body := `{"model":"claude-sonnet-4","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"Open the issue"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"curl -H 'X-Clawvisor-Caller: cv-nonce-abc123' http://localhost:25297/api/proxy/api.github.com/repos/x/y/issues"}}]}` +
		`]}`

	p := policies.NewInboundSanitize("http://localhost:25297/api/proxy", "http://localhost:25297")
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(body),
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody call, got %d", len(mut.ReplaceBodyCalls))
	}
	if got := verdict.AuditParams["inbound_history_sanitized"]; got != true {
		t.Errorf("audit field inbound_history_sanitized = %v, want true", got)
	}

	// The replaced body must no longer contain the rewritten artifacts.
	replaced := string(mut.ReplaceBodyCalls[0])
	if strings.Contains(replaced, "cv-nonce-abc123") {
		t.Errorf("cv-nonce still in replaced body:\n%s", replaced)
	}
	if strings.Contains(replaced, "X-Clawvisor-Caller") {
		t.Errorf("X-Clawvisor-Caller header still in replaced body:\n%s", replaced)
	}
	if strings.Contains(replaced, "http://localhost:25297/api/proxy") {
		t.Errorf("proxy URL still in replaced body:\n%s", replaced)
	}
}

// TestInboundSanitize_BothProviders verifies the policy fires for both
// supported providers.
func TestInboundSanitize_BothProviders(t *testing.T) {
	for _, provider := range []conversation.Provider{conversation.ProviderAnthropic, conversation.ProviderOpenAI} {
		t.Run(string(provider), func(t *testing.T) {
			p := policies.NewInboundSanitize("http://localhost:25297/api/proxy", "http://localhost:25297")
			var body []byte
			switch provider {
			case conversation.ProviderAnthropic:
				body = []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
			case conversation.ProviderOpenAI:
				body = []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
			}
			req := &stubReadOnlyRequest{provider: provider, rawBody: body}
			mut := &recordingRequestMutator{}
			verdict, err := p.Preprocess(context.Background(), req, mut)
			if err != nil {
				t.Fatalf("Preprocess: %v", err)
			}
			if verdict.Outcome != pipeline.OutcomeAllow {
				t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
			}
		})
	}
}
