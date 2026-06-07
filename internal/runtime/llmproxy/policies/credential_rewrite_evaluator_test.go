package policies_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

func TestCredentialRewriteEvaluator_SkipWhenNotConfigured(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "WebFetch", Input: json.RawMessage(`{}`)}

	t.Run("nil resolver", func(t *testing.T) {
		e := policies.NewCredentialRewriteEvaluator(nil)
		v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if v.Outcome != pipeline.OutcomeSkip {
			t.Errorf("Outcome = %q, want Skip", v.Outcome)
		}
	})

	t.Run("nil Inspector", func(t *testing.T) {
		e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
			return &policies.CredentialRewriteInputs{Inspector: nil}
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

func TestCredentialRewriteEvaluator_SkipNonCredentialed(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: &stubNonceCache{},
			AgentID:      "agent-1",
		}
	})
	// Plain shell command, no autovault placeholder → trigger miss → Skip.
	tu := conversation.ToolUse{
		ID:    "toolu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"ls /tmp"}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", v.Outcome)
	}
}

func TestCredentialRewriteEvaluator_DenyOnMissingNonceCache(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: nil, // intentionally missing
			AgentID:      "agent-1",
			RewriteOpts:  inspector.RewriteOpts{ResolverBaseURL: "http://localhost:25297/api/proxy"},
		}
	})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", v.Outcome)
	}
	if got := rewriteFactOutcome(v.Facts); got != "caller_nonce_unavailable" {
		t.Errorf("rewrite fact outcome = %v, want caller_nonce_unavailable", got)
	}
}

func TestCredentialRewriteEvaluator_DenyOnNonceMintError(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	cache := &stubNonceCache{mintErr: errors.New("redis down")}
	e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: cache,
			AgentID:      "agent-1",
			RewriteOpts:  inspector.RewriteOpts{ResolverBaseURL: "http://localhost:25297/api/proxy"},
		}
	})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", v.Outcome)
	}
	if got := rewriteFactOutcome(v.Facts); got != "caller_nonce_mint_failed" {
		t.Errorf("rewrite fact outcome = %v, want caller_nonce_mint_failed", got)
	}
	if strings.Contains(v.Reason, "redis down") {
		t.Fatalf("Reason leaked raw nonce backend error: %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "audit log") {
		t.Fatalf("Reason = %q, want generic audit-log guidance", v.Reason)
	}
}

func TestCredentialRewriteEvaluator_RewriteSuccess(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	cache := &stubNonceCache{minted: "cv-nonce-abc"}
	mut := &recordingMutator{}
	e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: cache,
			AgentID:      "agent-1",
			RewriteOpts:  inspector.RewriteOpts{ResolverBaseURL: "http://localhost:25297/api/proxy"},
		}
	})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}
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
		t.Errorf("nonce minted for agent = %q, want agent-1", cache.lastAgID)
	}
	if host := rewriteFactHost(v.Facts); host != "api.github.com" {
		t.Errorf("rewrite fact host = %v, want api.github.com", host)
	}
}

func TestCredentialRewriteEvaluator_MutatorFailurePropagates(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	mutErr := errors.New("mutator failed")
	e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: &stubNonceCache{minted: "cv-nonce-abc"},
			AgentID:      "agent-1",
			RewriteOpts:  inspector.RewriteOpts{ResolverBaseURL: "http://localhost:25297/api/proxy"},
		}
	})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}

	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &failingRewriteMutator{err: mutErr})
	if !errors.Is(err, mutErr) {
		t.Fatalf("Evaluate error = %v, want mutator failure", err)
	}
	if v.Outcome != "" {
		t.Fatalf("verdict on mutator error = %+v, want zero verdict", v)
	}
}

func TestCredentialRewriteEvaluator_AuditFactsOmitSensitiveMaterial(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	cache := &stubNonceCache{minted: "cv-nonce-secret"}
	e := policies.NewCredentialRewriteEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.CredentialRewriteInputs {
		return &policies.CredentialRewriteInputs{
			Inspector:    insp,
			CallerNonces: cache,
			AgentID:      "agent-1",
			RewriteOpts:  inspector.RewriteOpts{ResolverBaseURL: "http://localhost:25297/api/proxy"},
		}
	})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}

	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	rawFacts, err := json.Marshal(v.Facts)
	if err != nil {
		t.Fatalf("marshal facts: %v", err)
	}
	auditSurface := v.Reason + "\n" + string(rawFacts)
	for _, forbidden := range []string{
		"Authorization",
		"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		"cv-nonce-secret",
	} {
		if strings.Contains(auditSurface, forbidden) {
			t.Fatalf("audit facts/reason leaked %q: %s", forbidden, auditSurface)
		}
	}
}

type failingRewriteMutator struct {
	err error
}

func (m *failingRewriteMutator) RewriteArgs(json.RawMessage) error {
	return m.err
}

func (m *failingRewriteMutator) ReplaceWithText(string) error {
	return nil
}

func rewriteFactOutcome(facts []pipeline.EvaluationFact) string {
	for _, f := range facts {
		if rf, ok := f.(pipeline.RewriteFact); ok {
			return rf.Outcome
		}
	}
	return ""
}

func rewriteFactHost(facts []pipeline.EvaluationFact) string {
	for _, f := range facts {
		if rf, ok := f.(pipeline.RewriteFact); ok {
			return rf.TargetHost
		}
	}
	return ""
}
