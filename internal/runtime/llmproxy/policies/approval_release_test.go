package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestApprovalRelease_SkipsNilResolver pins the gate.
func TestApprovalRelease_SkipsNilResolver(t *testing.T) {
	p := policies.NewApprovalRelease(nil)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic}
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
}

// TestApprovalRelease_AllowWhenNotHandled verifies the no-release
// path: resolver returns Handled=false → Allow with no mutation.
func TestApprovalRelease_AllowWhenNotHandled(t *testing.T) {
	resolver := func(_ context.Context) llmproxy.ReleaseResult {
		return llmproxy.ReleaseResult{Handled: false}
	}
	p := policies.NewApprovalRelease(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("expected no mutation, got %d", len(mut.ReplaceBodyCalls))
	}
}

// TestApprovalRelease_ShortCircuitWhenHandled verifies the release
// path: Handled=true → ShortCircuit with the synthesized response.
func TestApprovalRelease_ShortCircuitWhenHandled(t *testing.T) {
	syntheticBody := []byte(`{"synthesized":"approved"}`)
	resolver := func(_ context.Context) llmproxy.ReleaseResult {
		return llmproxy.ReleaseResult{
			Handled:     true,
			HTTPStatus:  200,
			Body:        syntheticBody,
			ContentType: "application/json",
			Decision:    "allow",
			Outcome:     "released",
			Reason:      "approval matched",
		}
	}
	p := policies.NewApprovalRelease(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic}
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeShortCircuit {
		t.Errorf("Outcome = %q, want ShortCircuit", verdict.Outcome)
	}
	if verdict.ShortCircuit == nil {
		t.Fatalf("ShortCircuit should be populated")
	}
	if string(verdict.ShortCircuit.Body) != string(syntheticBody) {
		t.Errorf("synthetic body = %q, want %q", verdict.ShortCircuit.Body, syntheticBody)
	}
	if verdict.ShortCircuit.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", verdict.ShortCircuit.StatusCode)
	}
	if verdict.ShortCircuit.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type header missing")
	}
	if v := verdict.AuditParams["approval_release_handled"]; v != true {
		t.Errorf("approval_release_handled = %v, want true", v)
	}
	if v := verdict.AuditParams["approval_release_decision"]; v != "allow" {
		t.Errorf("approval_release_decision = %v, want allow", v)
	}
}

// TestApprovalRelease_DefaultsContentTypeAndStatus pins the defaults
// when the resolver returns Handled=true but doesn't fill in
// ContentType/HTTPStatus.
func TestApprovalRelease_DefaultsContentTypeAndStatus(t *testing.T) {
	resolver := func(_ context.Context) llmproxy.ReleaseResult {
		return llmproxy.ReleaseResult{
			Handled: true,
			Body:    []byte(`{}`),
		}
	}
	p := policies.NewApprovalRelease(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic}
	verdict, err := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.ShortCircuit.StatusCode != 200 {
		t.Errorf("default StatusCode = %d, want 200", verdict.ShortCircuit.StatusCode)
	}
	if verdict.ShortCircuit.Headers["Content-Type"] != "application/json" {
		t.Errorf("default Content-Type missing or wrong: %v", verdict.ShortCircuit.Headers)
	}
}
