package policies_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// Phase 6: synthetic third provider used to validate that the
// pipeline + policy abstractions accept a new provider without
// requiring edits to policies/ or pipeline/. Today this stub stays
// confined to tests; once a real third provider lands (Google,
// Bedrock, etc.), the new value moves into conversation/types.go.
const providerStubGoogle conversation.Provider = "google"

// TestPhase6_StubProvider_PipelineProcessesNewProvider validates the
// end-to-end Phase 6 promise: a request tagged with a brand-new
// Provider value flows through the preprocess pipeline without any
// policy returning an error or panicking. Most policies should emit
// Skip (the provider-specific ones like anthropic_sanitize) or
// fall through their generic paths (the provider-agnostic ones like
// inbound_sanitize, control_notice).
//
// When this test passes for a stub provider, adding a real provider
// reduces to: implement its parser + stream codecs in conversation/,
// add its enum value, wire its forwarder routing. Zero edits in
// policies/.
func TestPhase6_StubProvider_PipelineProcessesNewProvider(t *testing.T) {
	body := []byte(`{"model":"gemini-pro","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	// Use a subset of preprocess policies that don't require
	// provider-specific helpers — the parser-based ones (e.g.,
	// inline_task_intercept which calls newApprovalBodyEditor that
	// switches on provider) gracefully Skip for unknown providers.
	chain := []pipeline.RequestPolicy{
		policies.NewAnthropicSanitize(),     // expected Skip for stub provider
		policies.NewInboundSanitize("", ""), // expected Skip / Allow with no mutation
		policies.NewSecretHistoryStrip(),    // expected Allow with no mutation
		policies.NewSyntheticHistoryStrip(), // expected Allow with no mutation
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/google/generate", strings.NewReader(string(body)))
	req := &stubReadOnlyRequest{
		provider:        providerStubGoogle,
		rawBody:         body,
		userID:          "u1",
		agentID:         "a1",
		httpReqOverride: httpReq,
	}

	result, err := pipeline.RunPre(context.Background(), req, chain)
	if err != nil {
		t.Fatalf("pipeline rejected stub provider: %v", err)
	}

	if result.DenyReason != "" {
		t.Errorf("stub provider denied unexpectedly: %s by %s", result.DenyReason, result.DeniedBy)
	}
	if result.ShortCircuit != nil {
		t.Errorf("stub provider short-circuited unexpectedly: %+v", result.ShortCircuit)
	}

	// anthropic_sanitize must have Skipped (provider != Anthropic).
	for _, v := range result.Verdicts {
		if v.Name == "anthropic_sanitize" && v.Verdict.Outcome != pipeline.OutcomeSkip {
			t.Errorf("anthropic_sanitize should Skip for stub provider, got %q", v.Verdict.Outcome)
		}
	}

	// Body should pass through unchanged (no policy claimed the stub
	// provider's request).
	if string(result.FinalBody) != string(body) {
		t.Errorf("body modified unexpectedly\n--- want ---\n%s\n--- got ---\n%s", body, result.FinalBody)
	}
}

// TestPhase6_StubProvider_ToolUseEvaluatorChainAcceptsNewProvider
// validates the same for the per-tool-use chain: a synthetic
// ToolUseEvaluator pipeline runs against a tool_use without crashing
// or requiring provider-specific branches.
func TestPhase6_StubProvider_ToolUseEvaluatorChainAcceptsNewProvider(t *testing.T) {
	tools := []conversation.ToolUse{{
		ID:    "toolu_g1",
		Name:  "Bash",
		Input: json.RawMessage(`{"cmd":"ls"}`),
	}}

	// The inspector chain handles the trigger-miss → Skip path
	// uniformly across providers (it checks the input substring, not
	// the provider). TaskScopeEvaluator is provider-agnostic by
	// construction (the resolver closure carries identity).
	chain := []pipeline.ToolUseEvaluator{
		policies.NewInspectorChain(nil, nil), // nil inspector → Skip
		policies.NewTaskScopeEvaluator(nil),  // nil resolver → Skip
		policies.NewIntentVerifyEvaluator(nil),
		policies.NewPassThroughEvaluator(),
	}

	res := &chainIntegrationResponse{provider: providerStubGoogle}
	result, err := pipeline.EvaluateToolUses(
		context.Background(),
		res,
		tools,
		chain,
		func(string) pipeline.ToolUseMutator { return chainIntegrationMutator{} },
	)
	if err != nil {
		t.Fatalf("EvaluateToolUses rejected stub provider: %v", err)
	}

	// Gating evaluators Skip → explicit pass-through tail allows.
	if v := result.PerToolUse["toolu_g1"]; v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("stub provider tool_use Outcome = %q, want Allow (pass-through tail)", v.Outcome)
	}
}

// TestPhase6_StubProvider_StreamMutatorRejectsUnknownShape pins the
// boundary where provider-awareness legitimately lives: the response
// stream mutator. A new shape needs new codec wiring; the rejection
// is clear rather than silent.
func TestPhase6_StubProvider_StreamMutatorRejectsUnknownShape(t *testing.T) {
	var dst strings.Builder
	src := io.NopCloser(strings.NewReader(""))
	_, err := pipeline.NewStreamingResponseMutator(&dst, src, conversation.StreamShapeUnknown)
	if err == nil {
		t.Errorf("expected unknown stream shape to be rejected at construction")
	}
}
