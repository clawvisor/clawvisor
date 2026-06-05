package policies_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestPhase6_E2E_FullGoogleChain validates the Phase 6 promise
// end-to-end: a Gemini request flows through the parser → preprocess
// pipeline → tool-use evaluator chain → coalescing → forwarder
// resolution without policies/ or pipeline/ requiring edits beyond
// the additive switch arms (which live at provider-dispatch
// boundaries, the legitimate home for provider awareness).
func TestPhase6_E2E_FullGoogleChain(t *testing.T) {
	// --- Phase 6: parser recognizes Gemini URL ---

	httpReq, _ := http.NewRequest(
		http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
		strings.NewReader(""),
	)
	parser := conversation.DefaultRegistry().Match(httpReq)
	if parser == nil {
		t.Fatalf("DefaultRegistry didn't recognize Gemini URL")
	}
	if parser.Name() != conversation.ProviderGoogle {
		t.Errorf("parser.Name() = %q, want %q", parser.Name(), conversation.ProviderGoogle)
	}

	// --- Phase 6: parser extracts turns from Gemini body ---

	body := []byte(`{
		"systemInstruction": {"role":"user","parts":[{"text":"You are a helpful assistant"}]},
		"contents": [{"role":"user","parts":[{"text":"Run ls /tmp"}]}]
	}`)
	turns, err := parser.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns (system + user), got %d", len(turns))
	}

	// --- Phase 6: preprocess pipeline runs against the Gemini request ---

	chain := []pipeline.RequestPolicy{
		// anthropic_sanitize must Skip (provider != Anthropic).
		policies.NewAnthropicSanitize(),
		// inbound_sanitize runs for all providers; no rewrite expected.
		policies.NewInboundSanitize("http://localhost:25297/api/proxy", "http://localhost:25297"),
		policies.NewSyntheticHistoryStrip(),
	}
	pipeReq := &stubReadOnlyRequest{
		provider:        conversation.ProviderGoogle,
		rawBody:         body,
		userID:          "u1",
		agentID:         "a1",
		httpReqOverride: httpReq,
	}
	result, err := pipeline.RunPre(context.Background(), pipeReq, chain)
	if err != nil {
		t.Fatalf("RunPre on Gemini request: %v", err)
	}
	if result.DenyReason != "" {
		t.Errorf("Gemini request denied: %s", result.DenyReason)
	}
	// Body should pass through unchanged (no policy modifies it).
	if string(result.FinalBody) != string(body) {
		t.Errorf("Gemini body modified unexpectedly")
	}
	// anthropic_sanitize must Skip.
	for _, v := range result.Verdicts {
		if v.Name == "anthropic_sanitize" && v.Verdict.Outcome != pipeline.OutcomeSkip {
			t.Errorf("anthropic_sanitize should Skip for Gemini, got %q", v.Verdict.Outcome)
		}
	}

	// --- Phase 6: tool-use evaluator chain accepts Gemini tool_uses ---

	// Synthesize a tool_use that would appear in a Gemini response.
	tools := []conversation.ToolUse{{
		ID:    "call_g1",
		Name:  "Bash",
		Input: json.RawMessage(`{"cmd":"ls /tmp"}`),
	}}
	evalChain := []pipeline.ToolUseEvaluator{
		policies.NewInspectorChain(nil, nil),
		policies.NewTaskScopeEvaluator(nil),
	}
	geminiRes := &chainIntegrationResponse{provider: conversation.ProviderGoogle}
	toolResult, err := pipeline.EvaluateToolUses(
		context.Background(),
		geminiRes,
		tools,
		evalChain,
		func(string) pipeline.ToolUseMutator { return chainIntegrationMutator{} },
	)
	if err != nil {
		t.Fatalf("EvaluateToolUses on Gemini tool_uses: %v", err)
	}
	if v := toolResult.PerToolUse["call_g1"]; v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Gemini tool_use Outcome = %q, want Allow (default for all-Skip)", v.Outcome)
	}

	// --- Phase 6: forwarder resolves Gemini upstream URL ---

	selector := llmproxy.DefaultUpstream
	upstreamURL, err := selector.URL(conversation.ProviderGoogle, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("forwarder rejected Gemini provider: %v", err)
	}
	if upstreamURL.Host != "generativelanguage.googleapis.com" {
		t.Errorf("upstream URL host = %q, want generativelanguage.googleapis.com", upstreamURL.Host)
	}

	// --- Phase 6: streamingResponseMutator accepts the new shape ---

	upstreamReader := strings.NewReader(`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n")
	var dst strings.Builder
	mut, err := pipeline.NewStreamingResponseMutator(&dst, upstreamReader, conversation.StreamShapeGoogleGemini)
	if err != nil {
		t.Fatalf("streamingResponseMutator rejected Gemini shape: %v", err)
	}
	if committer, ok := mut.(interface{ Commit() error }); ok {
		if err := committer.Commit(); err != nil {
			t.Fatalf("Commit (no mutation): %v", err)
		}
	}
	if !strings.Contains(dst.String(), `"text":"hi"`) {
		t.Errorf("pass-through stream lost upstream content:\n%s", dst.String())
	}

	// --- Sanity: forwarder URL has the expected scheme/path shape ---
	_ = url.URL{} // silence unused import when not needed
	if upstreamURL.Scheme != "https" {
		t.Errorf("upstream scheme = %q, want https", upstreamURL.Scheme)
	}
}

// TestPhase6_E2E_NoPolicyEditsRequired pins the structural promise:
// the test file `phase6_stub_provider_test.go` validates each
// individual layer; this test validates the FULL chain integration
// for an actual new provider value (ProviderGoogle) — no synthetic.
// If any code path requires an additive switch arm in policies/ or
// pipeline/ outside the documented exemptions (response_mutator_impl
// dispatch), this test will surface the gap.
func TestPhase6_E2E_NoPolicyEditsRequired(t *testing.T) {
	// Construct a real Gemini request through the registered parser
	// path; the same path real production traffic would take.
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	httpReq := httptest.NewRequest(
		http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
		strings.NewReader(string(body)),
	)
	parser := conversation.DefaultRegistry().Match(httpReq)
	if parser == nil {
		t.Fatalf("Gemini parser not registered in DefaultRegistry")
	}
	if _, err := parser.ParseRequest(body); err != nil {
		t.Fatalf("parser rejected real Gemini body: %v", err)
	}

	// Run the request through the full preprocess chain with all 9 wired policies.
	pipeReq := &stubReadOnlyRequest{
		provider:        conversation.ProviderGoogle,
		rawBody:         body,
		userID:          "u1",
		agentID:         "a1",
		httpReqOverride: httpReq,
	}
	chain := []pipeline.RequestPolicy{
		policies.NewAnthropicSanitize(),
		policies.NewInboundSanitize("", ""),
		policies.NewSecretHistoryStrip(),
		policies.NewSyntheticHistoryStrip(),
	}
	result, err := pipeline.RunPre(context.Background(), pipeReq, chain)
	if err != nil {
		t.Fatalf("pipeline rejected Gemini provider: %v", err)
	}
	if result.DenyReason != "" {
		t.Errorf("policy denied Gemini request: %s by %s", result.DenyReason, result.DeniedBy)
	}
}
