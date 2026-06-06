package policies_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

func TestScriptSessionEvaluator_SkipWhenNotConfigured(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"curl https://example.com"}`)}

	t.Run("nil resolver", func(t *testing.T) {
		e := policies.NewScriptSessionEvaluator(nil)
		v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if v.Outcome != pipeline.OutcomeSkip {
			t.Errorf("Outcome = %q, want Skip", v.Outcome)
		}
	})

	t.Run("empty ResolverBaseURL", func(t *testing.T) {
		e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
			return &policies.ScriptSessionInputs{ResolverBaseURL: ""}
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

func TestScriptSessionEvaluator_SkipWhenNotScriptSession(t *testing.T) {
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy"}
	})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{"url":"https://api.github.com/repos/x/y/issues","headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (non-script-session call)", v.Outcome)
	}
}

func TestScriptSessionEvaluator_AllowWhenScriptSession(t *testing.T) {
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy"}
	})
	// Structured tool shape: URL targets resolver mount + script-session header.
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"http://localhost:25297/api/proxy/repos/x/y/issues",
			"headers":{
				"X-Clawvisor-Caller":"Bearer cv-script-abc123",
				"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
			}
		}`),
	}
	v, err := e.Evaluate(context.Background(), newStubResp(), tu, &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", v.Outcome)
	}
	found := false
	for _, f := range v.Facts {
		if ss, ok := f.(pipeline.ScriptSessionFact); ok && ss.Outcome == "script_session_passthrough" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ScriptSessionFact missing or wrong outcome (facts: %+v)", v.Facts)
	}
}
