package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// rewriterEvaluator queues a rewrite via the mutator and returns Rewrite.
type rewriterEvaluator struct {
	name     string
	newInput json.RawMessage
}

func (e *rewriterEvaluator) Name() string { return e.name }
func (e *rewriterEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, mut pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if err := mut.RewriteArgs(e.newInput); err != nil {
		return pipeline.ToolUseVerdict{}, err
	}
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeRewrite, Reason: "rewritten"}, nil
}

// replacerEvaluator queues a text replacement via the mutator and returns Deny.
type replacerEvaluator struct {
	name string
	text string
}

func (e *replacerEvaluator) Name() string { return e.name }
func (e *replacerEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, mut pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if err := mut.ReplaceWithText(e.text); err != nil {
		return pipeline.ToolUseVerdict{}, err
	}
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeDeny, Reason: "replaced"}, nil
}

// TestBridgeToolUseEvaluator_AllowAndRewrite pins that:
//   - OutcomeAllow → Allowed=true with no mutations
//   - OutcomeRewrite + mutator.RewriteArgs → Allowed=true with RewriteInput set
func TestBridgeToolUseEvaluator_AllowAndRewrite(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{
		{ID: "toolu_allow", Name: "Read", Input: json.RawMessage(`{}`)},
		{ID: "toolu_rewrite", Name: "WebFetch", Input: json.RawMessage(`{}`)},
	}

	allowOnce := &allowEvaluator{name: "allow", tag: "ok"}
	rewriter := &rewriterEvaluator{
		name:     "rewriter",
		newInput: json.RawMessage(`{"url":"https://resolver/api"}`),
	}

	// Use a small chain: skip on the first tool_use (so it falls through
	// to Allow), rewrite on the second. To pick per-tool, use a switching
	// evaluator that consults the tool ID.
	switcher := &perToolEvaluator{
		name: "switcher",
		byID: map[string]pipeline.ToolUseEvaluator{
			"toolu_allow":   allowOnce,
			"toolu_rewrite": rewriter,
		},
	}

	eval, result, err := pipeline.RunToolUseEvaluators(
		context.Background(),
		res,
		tools,
		[]pipeline.ToolUseEvaluator{switcher},
	)
	if err != nil {
		t.Fatalf("RunToolUseEvaluators: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}

	vAllow := eval(tools[0])
	if !vAllow.Allowed {
		t.Errorf("toolu_allow: Allowed = false, want true")
	}
	if len(vAllow.RewriteInput) != 0 {
		t.Errorf("toolu_allow: RewriteInput = %q, want empty", string(vAllow.RewriteInput))
	}

	vRewrite := eval(tools[1])
	if !vRewrite.Allowed {
		t.Errorf("toolu_rewrite: Allowed = false on Rewrite outcome, want true")
	}
	if string(vRewrite.RewriteInput) != `{"url":"https://resolver/api"}` {
		t.Errorf("toolu_rewrite: RewriteInput = %q", string(vRewrite.RewriteInput))
	}
}

// TestBridgeToolUseEvaluator_DenyWithSubstitute pins that:
//   - OutcomeDeny → Allowed=false
//   - mutator.ReplaceWithText → SubstituteWith populated
func TestBridgeToolUseEvaluator_DenyWithSubstitute(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_x", Name: "Bash"}}

	evaluators := []pipeline.ToolUseEvaluator{
		&replacerEvaluator{name: "replace", text: "denied: please approve via /control/tasks"},
	}

	eval, _, err := pipeline.RunToolUseEvaluators(context.Background(), res, tools, evaluators)
	if err != nil {
		t.Fatalf("RunToolUseEvaluators: %v", err)
	}

	v := eval(tools[0])
	if v.Allowed {
		t.Errorf("Allowed = true, want false")
	}
	if v.SubstituteWith != "denied: please approve via /control/tasks" {
		t.Errorf("SubstituteWith = %q", v.SubstituteWith)
	}
	if v.Reason != "replaced" {
		t.Errorf("Reason = %q, want \"replaced\"", v.Reason)
	}
}

// TestBridgeToolUseEvaluator_ContinueStaysStructured pins that a
// ContinueSignal remains structured on the triggering tool_use's
// verdict; final adapters derive flat provider content only at the
// boundary.
func TestBridgeToolUseEvaluator_ContinueStaysStructured(t *testing.T) {
	res := &orchTestResponse{provider: conversation.ProviderAnthropic}
	tools := []conversation.ToolUse{{ID: "toolu_local", Name: "Bash"}}

	cont := &continueWithResultEvaluator{
		name:    "cont",
		results: []json.RawMessage{json.RawMessage(`{"type":"tool_result","content":"ok"}`)},
		notice:  "auto-approved",
	}

	eval, result, err := pipeline.RunToolUseEvaluators(context.Background(), res, tools, []pipeline.ToolUseEvaluator{cont})
	if err != nil {
		t.Fatalf("RunToolUseEvaluators: %v", err)
	}
	if result.Continue == nil {
		t.Fatal("result.Continue is nil")
	}

	v := eval(tools[0])
	if v.Continue == nil {
		t.Fatal("verdict Continue is nil")
	}
	if v.ContinueWithToolResult != "" {
		t.Errorf("ContinueWithToolResult = %q, want empty compatibility field", v.ContinueWithToolResult)
	}
	if content, ok := v.ContinuationToolResultContent(); !ok || content != "ok" {
		t.Errorf("ContinuationToolResultContent = %q, %v; want ok, true", content, ok)
	}
	if got := v.ContinuationNotice(); got != "auto-approved" {
		t.Errorf("ContinuationNotice = %q", got)
	}
}

// perToolEvaluator dispatches to a per-tool-ID inner evaluator. Used by
// the bridge tests to drive different verdicts per tool_use in one chain.
type perToolEvaluator struct {
	name string
	byID map[string]pipeline.ToolUseEvaluator
}

func (e *perToolEvaluator) Name() string { return e.name }
func (e *perToolEvaluator) Evaluate(ctx context.Context, res pipeline.ReadOnlyResponse, tu conversation.ToolUse, mut pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	inner, ok := e.byID[tu.ID]
	if !ok {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	return inner.Evaluate(ctx, res, tu, mut)
}

// continueWithResultEvaluator returns a ContinueSignal carrying both
// a tool-result block and a prepend notice.
type continueWithResultEvaluator struct {
	name    string
	results []json.RawMessage
	notice  string
}

func (e *continueWithResultEvaluator) Name() string { return e.name }
func (e *continueWithResultEvaluator) Evaluate(_ context.Context, _ pipeline.ReadOnlyResponse, _ conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Continue: &pipeline.ContinueSignal{
			SyntheticToolResults: e.results,
			PrependNotice:        e.notice,
		},
	}, nil
}
