package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestSecretHold_SkipsNilResolver pins the gate.
func TestSecretHold_SkipsNilResolver(t *testing.T) {
	p := policies.NewSecretHold(nil)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}
	v, err := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", v.Outcome)
	}
}

// TestSecretHold_AllowWhenNotHeld pins the no-hold path.
func TestSecretHold_AllowWhenNotHeld(t *testing.T) {
	resolver := func(_ context.Context, _ []byte) policies.SecretHoldResult {
		return policies.SecretHoldResult{Held: false}
	}
	p := policies.NewSecretHold(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}
	v, err := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", v.Outcome)
	}
}

// TestSecretHold_ShortCircuitOnHold pins the hold path: short-circuit
// with the synthesized hold-prompt response.
func TestSecretHold_ShortCircuitOnHold(t *testing.T) {
	syntheticBody := []byte(`{"hold":"secret_pending"}`)
	resolver := func(_ context.Context, _ []byte) policies.SecretHoldResult {
		return policies.SecretHoldResult{
			Held:        true,
			HTTPStatus:  200,
			Body:        syntheticBody,
			ContentType: "application/json",
			Decision:    "hold",
			Outcome:     "secret_pending",
			Reason:      "user must adjudicate",
		}
	}
	p := policies.NewSecretHold(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{"secret":"ghp_xyz"}`)}

	v, err := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if v.Outcome != pipeline.OutcomeShortCircuit {
		t.Errorf("Outcome = %q, want ShortCircuit", v.Outcome)
	}
	if v.ShortCircuit == nil {
		t.Fatalf("ShortCircuit should be populated")
	}
	if string(v.ShortCircuit.Body) != string(syntheticBody) {
		t.Errorf("body = %q, want %q", v.ShortCircuit.Body, syntheticBody)
	}
	if v.ShortCircuit.StatusCode != 200 {
		t.Errorf("status = %d, want 200", v.ShortCircuit.StatusCode)
	}
	if v.ShortCircuit.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type header missing")
	}
	if v.AuditParams["secret_hold_held"] != true {
		t.Errorf("secret_hold_held = %v, want true", v.AuditParams["secret_hold_held"])
	}
	if v.AuditParams["secret_hold_outcome"] != "secret_pending" {
		t.Errorf("secret_hold_outcome = %v, want secret_pending", v.AuditParams["secret_hold_outcome"])
	}
}

// TestSecretHold_DefaultsApplied pins the defaults.
func TestSecretHold_DefaultsApplied(t *testing.T) {
	resolver := func(_ context.Context, _ []byte) policies.SecretHoldResult {
		return policies.SecretHoldResult{Held: true, Body: []byte(`{}`)}
	}
	p := policies.NewSecretHold(resolver)
	req := &stubReadOnlyRequest{provider: conversation.ProviderAnthropic, rawBody: []byte(`{}`)}
	v, _ := p.Preprocess(context.Background(), req, &recordingRequestMutator{})
	if v.ShortCircuit.StatusCode != 200 {
		t.Errorf("default StatusCode = %d, want 200", v.ShortCircuit.StatusCode)
	}
	if v.ShortCircuit.Headers["Content-Type"] != "application/json" {
		t.Errorf("default Content-Type missing")
	}
}
