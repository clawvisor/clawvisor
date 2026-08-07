package policies_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

type stubNonceCache struct {
	mintErr       error
	minted        string
	lastTgt       llmproxy.NonceTarget
	lastAgID      string
	consumed      int
	consumedTgt   llmproxy.NonceTarget
	consumedNonce string
}

func (s *stubNonceCache) Mint(_ context.Context, agentID string, tgt llmproxy.NonceTarget) (string, error) {
	s.lastAgID = agentID
	s.lastTgt = tgt
	if s.mintErr != nil {
		return "", s.mintErr
	}
	if s.minted == "" {
		return "cv-nonce-stub", nil
	}
	return s.minted, nil
}

func (s *stubNonceCache) Consume(_ context.Context, nonce string, tgt llmproxy.NonceTarget) (string, error) {
	s.consumed++
	s.consumedNonce = nonce
	s.consumedTgt = tgt
	return "", nil
}

type recordingMutator struct {
	rewrites     []json.RawMessage
	replacements []string
}

func (m *recordingMutator) RewriteArgs(in json.RawMessage) error {
	m.rewrites = append(m.rewrites, append(json.RawMessage(nil), in...))
	return nil
}
func (m *recordingMutator) ReplaceWithText(text string) error {
	m.replacements = append(m.replacements, text)
	return nil
}

func newStubResp() *stubReadOnlyResponse {
	return &stubReadOnlyResponse{provider: conversation.ProviderAnthropic}
}

// TestControlToolUseEvaluator_SkipWhenNoControlConfigured pins that an
// empty ControlBaseURL or nil resolver makes the evaluator a no-op.
func TestControlToolUseEvaluator_SkipWhenNoControlConfigured(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}

	t.Run("nil resolver", func(t *testing.T) {
		e := policies.NewControlToolUseEvaluator(nil)
		v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if v.Outcome != pipeline.OutcomeSkip {
			t.Errorf("Outcome = %q, want Skip", v.Outcome)
		}
	})

	t.Run("resolver returns nil inputs", func(t *testing.T) {
		e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
			return nil
		})
		v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if v.Outcome != pipeline.OutcomeSkip {
			t.Errorf("Outcome = %q, want Skip", v.Outcome)
		}
	})

	t.Run("empty ControlBaseURL", func(t *testing.T) {
		e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
			return &policies.ControlToolUseInputs{ControlBaseURL: ""}
		})
		v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if v.Outcome != pipeline.OutcomeSkip {
			t.Errorf("Outcome = %q, want Skip", v.Outcome)
		}
	})
}

// TestControlToolUseEvaluator_SkipWhenNotControlPlane pins that a
// non-control tool_use passes through without claiming the chain.
func TestControlToolUseEvaluator_SkipWhenNotControlPlane(t *testing.T) {
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   &stubNonceCache{},
		}
	})
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues","method":"GET"}`),
	}
	mut := &recordingMutator{}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, mut)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", v.Outcome)
	}
	if len(mut.rewrites) != 0 {
		t.Errorf("rewrites = %d, want 0", len(mut.rewrites))
	}
}

// TestControlToolUseEvaluator_DenyOnMissingNonceCache pins the
// safe-fail behavior when ControlBaseURL is configured but no nonce
// cache is — refusing rather than embedding the agent's raw token.
func TestControlToolUseEvaluator_DenyOnMissingNonceCache(t *testing.T) {
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   nil,
		}
	})
	// A well-formed control curl pointing at clawvisor.local/control/tasks.
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' -H 'Content-Type: application/json' --data '{}'"}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", v.Outcome)
	}
	if got := controlFactOutcome(v.Facts); got != "caller_nonce_unavailable" {
		t.Errorf("control fact outcome = %v, want caller_nonce_unavailable", got)
	}
}

// TestControlToolUseEvaluator_DenyOnNonceMintError pins the
// nonce-mint-error path.
func TestControlToolUseEvaluator_DenyOnNonceMintError(t *testing.T) {
	cache := &stubNonceCache{mintErr: errors.New("redis down")}
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   cache,
		}
	})
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'"}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", v.Outcome)
	}
	if got := controlFactOutcome(v.Facts); got != "caller_nonce_mint_failed" {
		t.Errorf("control fact outcome = %v, want caller_nonce_mint_failed", got)
	}
	if strings.Contains(v.Reason, "redis down") {
		t.Fatalf("Reason leaked raw nonce backend error: %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "audit log") {
		t.Fatalf("Reason = %q, want generic audit-log guidance", v.Reason)
	}
}

func controlFactOutcome(facts []pipeline.EvaluationFact) string {
	for _, f := range facts {
		if cf, ok := f.(pipeline.ControlFact); ok {
			return cf.Outcome
		}
	}
	return ""
}

// TestControlToolUseEvaluator_RewriteSuccess pins the happy path: a
// well-formed control curl → nonce minted, args rewritten, OutcomeRewrite.
func TestControlToolUseEvaluator_RewriteSuccess(t *testing.T) {
	cache := &stubNonceCache{minted: "cv-nonce-abc123"}
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   cache,
		}
	})
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS -X POST 'https://clawvisor.local/control/tasks' -H 'Content-Type: application/json' --data '{\"purpose\":\"x\"}'"}`),
	}
	mut := &recordingMutator{}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, mut)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeRewrite {
		t.Fatalf("Outcome = %q (Reason: %s), want Rewrite", v.Outcome, v.Reason)
	}
	if len(mut.rewrites) != 1 {
		t.Errorf("rewrites = %d, want 1", len(mut.rewrites))
	}
	if cache.lastAgID != "agent-1" {
		t.Errorf("nonce minted for %q, want agent-1", cache.lastAgID)
	}
	if cache.consumed != 0 {
		t.Errorf("nonce consumed on successful rewrite = %d, want 0", cache.consumed)
	}
}

func TestControlToolUseEvaluator_ConsumesNonceOnMutatorFailure(t *testing.T) {
	cache := &stubNonceCache{minted: "cv-nonce-abc123"}
	mutErr := errors.New("mutator failed")
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   cache,
		}
	})
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS -X POST 'https://clawvisor.local/control/tasks' -H 'Content-Type: application/json' --data '{\"purpose\":\"x\"}'"}`),
	}
	if _, err := e.Evaluate(context.Background(), newStubResp(), tu, &failingRewriteMutator{err: mutErr}); !errors.Is(err, mutErr) {
		t.Fatalf("Evaluate error = %v, want mutator failure", err)
	}
	if cache.consumed != 1 {
		t.Fatalf("nonce consumed = %d, want 1", cache.consumed)
	}
	if cache.consumedNonce != "cv-nonce-abc123" {
		t.Fatalf("consumed nonce = %q", cache.consumedNonce)
	}
	if cache.consumedTgt != cache.lastTgt {
		t.Fatalf("consumed target = %+v, want minted target %+v", cache.consumedTgt, cache.lastTgt)
	}
}

// TestControlToolUseEvaluator_InlineInterceptClaimsCall pins that when
// the InterceptInline hook claims the call, its verdict propagates
// directly and the regular rewrite path is skipped.
func TestControlToolUseEvaluator_InlineInterceptClaimsCall(t *testing.T) {
	claimed := pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeDeny,
		Reason:  "Reply `yes` or `y`",
	}
	cache := &stubNonceCache{}
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   cache,
			InterceptInline: func(_ context.Context, _ conversation.ToolUse, _ llmproxy.ControlCall) (pipeline.ToolUseVerdict, bool) {
				return claimed, true
			},
		}
	})
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'"}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny || v.Reason != "Reply `yes` or `y`" {
		t.Errorf("verdict = %+v, want claimed", v)
	}
	if cache.lastAgID != "" {
		t.Errorf("nonce mint should not have been called (got agent=%q)", cache.lastAgID)
	}
}

func TestControlToolUseEvaluator_MethodMismatchRedirectsToFailure(t *testing.T) {
	cache := &stubNonceCache{minted: "cv-nonce-failure-abc"}
	e := policies.NewControlToolUseEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ControlToolUseInputs {
		return &policies.ControlToolUseInputs{
			ControlBaseURL: "http://localhost:25297",
			AgentID:        "agent-1",
			CallerNonces:   cache,
		}
	})
	// A bodyless complete curl has no -X POST, so its actual method is GET.
	// But /complete requires POST. This triggers method mismatch.
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS 'https://clawvisor.local/control/tasks/task-xyz/complete'"}`),
	}
	mut := &recordingMutator{}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, mut)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeRewrite {
		t.Fatalf("Outcome = %q (Reason: %s), want Rewrite to failure path", v.Outcome, v.Reason)
	}
	if !strings.Contains(v.Reason, "method mismatch") {
		t.Errorf("expected reason to mention method mismatch, got: %q", v.Reason)
	}
	if len(mut.rewrites) != 1 {
		t.Errorf("rewrites = %d, want 1", len(mut.rewrites))
	}
	// Verify it targeted the failure endpoint
	if cache.lastTgt.Path != "/api/control/failure" {
		t.Errorf("expected failure path rewrite target, got %+v", cache.lastTgt)
	}
}
