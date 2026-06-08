package policies_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestAnthropicSanitize_SkipsNonAnthropic pins the provider-gate:
// OpenAI requests get OutcomeSkip with no mutations queued.
func TestAnthropicSanitize_SkipsNonAnthropic(t *testing.T) {
	p := policies.NewAnthropicSanitize()
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderOpenAI,
		rawBody:  []byte(`{"model":"gpt-4o","messages":[]}`),
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutations, got %d ReplaceBody calls", len(mut.ReplaceBodyCalls))
	}
}

// TestAnthropicSanitize_AllowWithoutMutation pins the clean-body path:
// a well-formed body with no empty text blocks gets Allow + no mutation.
func TestAnthropicSanitize_AllowWithoutMutation(t *testing.T) {
	p := policies.NewAnthropicSanitize()
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
		t.Errorf("expected no mutations on clean body, got %d", len(mut.ReplaceBodyCalls))
	}
	if _, ok := verdict.AuditParams["anthropic_empty_text_sanitized"]; ok {
		t.Errorf("audit field should be absent when not sanitized")
	}
}

// TestAnthropicSanitize_QueuesReplaceBodyWhenSanitized pins the
// mutation path: a body with an empty text block triggers ReplaceBody
// and the audit flag.
func TestAnthropicSanitize_QueuesReplaceBodyWhenSanitized(t *testing.T) {
	// Empty text block at message[0].content[0] — should get dropped.
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"hello"}]}]}`)
	p := policies.NewAnthropicSanitize()
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  body,
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if got := verdict.AuditParams["anthropic_empty_text_sanitized"]; got != true {
		t.Errorf("audit field anthropic_empty_text_sanitized = %v, want true", got)
	}
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody call, got %d", len(mut.ReplaceBodyCalls))
	}
	// The replaced body should still contain "hello" but not the empty text block.
	replaced := mut.ReplaceBodyCalls[0]
	if !bytes.Contains(replaced, []byte(`"hello"`)) {
		t.Errorf("replaced body lost non-empty content:\n%s", replaced)
	}
	// And it shouldn't have an empty text block lurking.
	if strings.Count(string(replaced), `"text":""`) != 0 {
		t.Errorf("replaced body still contains empty text block:\n%s", replaced)
	}
}

// TestAnthropicSanitize_DenyOnMalformedBody pins the parse-failure
// path: a body that doesn't parse as Anthropic Messages produces Deny.
func TestAnthropicSanitize_DenyOnMalformedBody(t *testing.T) {
	p := policies.NewAnthropicSanitize()
	req := &stubReadOnlyRequest{
		provider: conversation.ProviderAnthropic,
		rawBody:  []byte(`{not valid json`),
	}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess returned err (should be in Verdict.Reason): %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", verdict.Outcome)
	}
	if verdict.Reason == "" {
		t.Errorf("expected Reason populated on Deny")
	}
	if got, _ := verdict.AuditParams["anthropic_sanitize_error"].(string); got == "" || !strings.Contains(got, "invalid character") {
		t.Errorf("anthropic_sanitize_error audit field = %q, want raw parse detail", got)
	}
	if strings.Contains(verdict.Reason, "invalid character") {
		t.Errorf("deny reason = %q, should not expose raw parse detail", verdict.Reason)
	}
}
