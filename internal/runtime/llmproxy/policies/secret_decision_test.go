package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestSecretDecision_SkipsNilResolver pins the gate.
func TestSecretDecision_SkipsNilResolver(t *testing.T) {
	p := policies.NewSecretDecision(nil)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}
	v, err := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", v.Outcome)
	}
}

// TestSecretDecision_AllowWithoutMutation pins the no-decision path.
func TestSecretDecision_AllowWithoutMutation(t *testing.T) {
	resolver := func(_ context.Context, _ []byte) policies.SecretDecisionResult {
		return policies.SecretDecisionResult{} // empty result, no Handled, no ModifiedBody
	}
	p := policies.NewSecretDecision(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}
	mut := &recordingRequestMutator{}

	v, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", v.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutation, got %d", len(mut.ReplaceBodyCalls))
	}
}

// TestSecretDecision_BodyRewritePath pins the rewrite-without-handle
// path: user's decision modifies the body but the request still flows
// to upstream.
func TestSecretDecision_BodyRewritePath(t *testing.T) {
	newBody := []byte(`{"after_decision":"redacted"}`)
	resolver := func(_ context.Context, _ []byte) policies.SecretDecisionResult {
		return policies.SecretDecisionResult{
			Handled:      false,
			ModifiedBody: newBody,
			Action:       llmproxy.SecretDecisionDiscard,
			Outcome:      "discarded",
		}
	}
	p := policies.NewSecretDecision(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{"secret":"x"}`)}
	mut := &recordingRequestMutator{}

	v, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", v.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody, got %d", len(mut.ReplaceBodyCalls))
	}
	if string(mut.ReplaceBodyCalls[0]) != string(newBody) {
		t.Errorf("ReplaceBody body = %q, want %q", mut.ReplaceBodyCalls[0], newBody)
	}
	if v.AuditParams["secret_decision_action"] != string(llmproxy.SecretDecisionDiscard) {
		t.Errorf("audit action = %v, want %s", v.AuditParams["secret_decision_action"], llmproxy.SecretDecisionDiscard)
	}
}

// TestSecretDecision_ShortCircuitOnHandled pins the handled path:
// resolver returns Handled=true → ShortCircuit with synthesized response.
func TestSecretDecision_ShortCircuitOnHandled(t *testing.T) {
	syntheticBody := []byte(`{"decision":"vaulted"}`)
	resolver := func(_ context.Context, _ []byte) policies.SecretDecisionResult {
		return policies.SecretDecisionResult{
			Handled:     true,
			HTTPStatus:  200,
			Body:        syntheticBody,
			ContentType: "application/json",
			Action:      llmproxy.SecretDecisionVault,
			Decision:    "allow",
			Outcome:     "vaulted_and_continued",
		}
	}
	p := policies.NewSecretDecision(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}

	v, err := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeShortCircuit {
		t.Errorf("Outcome = %q, want ShortCircuit", v.Outcome)
	}
	if string(v.ShortCircuit.Body) != string(syntheticBody) {
		t.Errorf("synthetic body wrong")
	}
}
