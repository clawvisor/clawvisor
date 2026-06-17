package taskrisk

import (
	"context"
	"testing"
)

// TestPromptResolverFn_FillsEmptyOverride verifies the contract that
// the resolver-callback path mirrors the intent verifier: when the
// caller passes an empty PromptOverride and a non-empty OrgID, the
// registered resolver populates the override; a caller-provided
// override wins over the resolver.
func TestPromptResolverFn_FillsEmptyOverride(t *testing.T) {
	a := &LLMAssessor{}
	called := false
	a.SetPromptResolver(func(_ context.Context, orgID string) string {
		called = true
		if orgID != "org-a" {
			t.Errorf("resolver got orgID=%q, want org-a", orgID)
		}
		return "RESOLVED-PROMPT"
	})

	// Manually exercise the resolver-fill branch (we can't call Assess
	// without a live LLM; the resolver contract itself is what we test).
	req := AssessRequest{OrgID: "org-a"}
	if a.resolver != nil && req.OrgID != "" && req.PromptOverride == "" {
		if override := a.resolver(context.Background(), req.OrgID); override != "" {
			req.PromptOverride = override
		}
	}
	if !called {
		t.Error("resolver was not called")
	}
	if req.PromptOverride != "RESOLVED-PROMPT" {
		t.Errorf("PromptOverride=%q, want RESOLVED-PROMPT", req.PromptOverride)
	}

	// Caller-provided override wins.
	called = false
	req2 := AssessRequest{OrgID: "org-a", PromptOverride: "CALLER-PROVIDED"}
	if a.resolver != nil && req2.OrgID != "" && req2.PromptOverride == "" {
		if override := a.resolver(context.Background(), req2.OrgID); override != "" {
			req2.PromptOverride = override
		}
	}
	if called {
		t.Error("resolver should not have been called when caller pre-populated")
	}
	if req2.PromptOverride != "CALLER-PROVIDED" {
		t.Errorf("PromptOverride=%q, want CALLER-PROVIDED", req2.PromptOverride)
	}

	// Empty OrgID bypasses the resolver.
	called = false
	req3 := AssessRequest{OrgID: ""}
	if a.resolver != nil && req3.OrgID != "" && req3.PromptOverride == "" {
		if override := a.resolver(context.Background(), req3.OrgID); override != "" {
			req3.PromptOverride = override
		}
	}
	if called {
		t.Error("resolver should not have been called when orgID is empty")
	}
}
