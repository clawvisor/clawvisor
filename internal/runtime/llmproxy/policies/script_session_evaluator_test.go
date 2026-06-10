package policies_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptjudge"
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
		ID:    "toolu_1",
		Name:  "WebFetch",
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

// urlUnrecognizedToolUse is the canonical shape that triggers
// URLUnrecognized: cv-script token and autovault placeholder in the
// command, but the URL is hidden behind a shell variable so the
// literal-prefix recognizer can't see it.
func urlUnrecognizedToolUse() conversation.ToolUse {
	return conversation.ToolUse{
		ID:   "toolu_unr",
		Name: "Bash",
		Input: json.RawMessage(`{"command":"B='http://localhost:25297/api/proxy/x'\nC='X-Clawvisor-Caller: Bearer cv-script-abc'\nA='Authorization: Bearer autovault_y'\ncurl \"$B\" -H \"$C\" -H \"$A\""}`),
	}
}

type stubJudge struct {
	verdict scriptjudge.Verdict
	err     error
	last    scriptjudge.Input
}

func (s *stubJudge) Judge(_ context.Context, in scriptjudge.Input) (scriptjudge.Verdict, error) {
	s.last = in
	return s.verdict, s.err
}

// TestScriptSessionEvaluator_URLUnrecognized_NoJudge confirms the
// evaluator Skips (falls through to inspector chain) when the
// deterministic recognizer flags URL-unrecognized but no judge is
// wired.
func TestScriptSessionEvaluator_URLUnrecognized_NoJudge(t *testing.T) {
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy"}
	})
	v, err := e.Evaluate(context.Background(), newStubResp(), urlUnrecognizedToolUse(), &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (no judge configured)", v.Outcome)
	}
}

// TestScriptSessionEvaluator_URLUnrecognized_JudgeAllow confirms the
// judge's Allow verdict produces OutcomeAllow with the
// judge_allow ScriptSessionFact for audit + threads forensic fields
// (prompt SHA, latency, token usage) through so audit consumers can
// roll them up.
func TestScriptSessionEvaluator_URLUnrecognized_JudgeAllow(t *testing.T) {
	judge := &stubJudge{verdict: scriptjudge.Verdict{
		Allow:        true,
		Reason:       "variable holds the resolver URL",
		PromptSHA:    "abc123",
		LatencyMS:    47,
		InputTokens:  1234,
		OutputTokens: 56,
	}}
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy", Judge: judge}
	})
	v, err := e.Evaluate(context.Background(), newStubResp(), urlUnrecognizedToolUse(), &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", v.Outcome)
	}
	if judge.last.CVScriptToken == "" {
		t.Errorf("expected cv-script token to be extracted and passed to judge")
	}
	if judge.last.ResolverBaseURL == "" {
		t.Errorf("expected resolver base URL to be passed to judge")
	}
	var fact pipeline.ScriptSessionFact
	for _, f := range v.Facts {
		if ss, ok := f.(pipeline.ScriptSessionFact); ok && ss.Outcome == "script_session_judge_allow" {
			fact = ss
		}
	}
	if fact.Outcome == "" {
		t.Fatalf("ScriptSessionFact judge_allow missing (facts: %+v)", v.Facts)
	}
	if fact.JudgePromptSHA != "abc123" {
		t.Errorf("JudgePromptSHA = %q, want abc123", fact.JudgePromptSHA)
	}
	if fact.JudgeLatencyMS != 47 {
		t.Errorf("JudgeLatencyMS = %d, want 47", fact.JudgeLatencyMS)
	}
	if fact.JudgeInputTokens != 1234 || fact.JudgeOutputTokens != 56 {
		t.Errorf("token counts = (%d,%d), want (1234,56)", fact.JudgeInputTokens, fact.JudgeOutputTokens)
	}
}

// TestScriptSessionEvaluator_URLUnrecognized_JudgeBlock confirms the
// judge's Block verdict produces OutcomeDeny with the agent's
// guidance text propagated into Reason and the judge_block
// ScriptSessionFact for audit.
func TestScriptSessionEvaluator_URLUnrecognized_JudgeBlock(t *testing.T) {
	judge := &stubJudge{verdict: scriptjudge.Verdict{
		Allow:         false,
		Reason:        "URL targets gmail.googleapis.com directly",
		AgentGuidance: "replace https://gmail.googleapis.com with http://localhost:25297/api/proxy/gmail/v1/...",
	}}
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy", Judge: judge}
	})
	v, err := e.Evaluate(context.Background(), newStubResp(), urlUnrecognizedToolUse(), &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", v.Outcome)
	}
	if !strings.Contains(v.Reason, "replace https://gmail.googleapis.com") {
		t.Errorf("Reason %q should carry agent guidance verbatim", v.Reason)
	}
	if !strings.HasPrefix(v.Reason, "Clawvisor: script-session call refused — ") {
		t.Errorf("Reason %q should have Clawvisor refusal prefix", v.Reason)
	}
	if v.SubstituteWith != v.Reason {
		t.Errorf("SubstituteWith = %q, want = Reason for the terminal-fallback path", v.SubstituteWith)
	}
	if v.Continue == nil || len(v.Continue.SyntheticToolResults) != 1 {
		t.Fatalf("Continue.SyntheticToolResults missing — judge block should flow guidance back to the agent as a recoverable tool_result")
	}
	if content, ok := v.ContinuationToolResultContent(); !ok || content != v.Reason {
		t.Errorf("ContinuationToolResultContent = %q, %v; want Reason verbatim", content, ok)
	}
	found := false
	for _, f := range v.Facts {
		if ss, ok := f.(pipeline.ScriptSessionFact); ok && ss.Outcome == "script_session_judge_block" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ScriptSessionFact judge_block missing (facts: %+v)", v.Facts)
	}
}

// TestScriptSessionEvaluator_URLUnrecognized_JudgeError confirms the
// evaluator Skips (falls through to inspector chain) when the judge
// transport/parse errors. Empty guidance + judge error both fall
// through; the inspector's generic refusal is the safer fallback
// than acting on a half-baked verdict. The judge_error fact is still
// emitted so the audit row shows the attempt + latency, even though
// the evaluator declined to use the verdict.
func TestScriptSessionEvaluator_URLUnrecognized_JudgeError(t *testing.T) {
	judge := &stubJudge{
		verdict: scriptjudge.Verdict{PromptSHA: "abc123", LatencyMS: 31},
		err:     errors.New("transient transport failure"),
	}
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy", Judge: judge}
	})
	v, err := e.Evaluate(context.Background(), newStubResp(), urlUnrecognizedToolUse(), &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip (judge errored)", v.Outcome)
	}
	var fact pipeline.ScriptSessionFact
	for _, f := range v.Facts {
		if ss, ok := f.(pipeline.ScriptSessionFact); ok && ss.Outcome == "script_session_judge_error" {
			fact = ss
		}
	}
	if fact.Outcome == "" {
		t.Fatalf("ScriptSessionFact judge_error missing (facts: %+v)", v.Facts)
	}
	if !strings.Contains(fact.JudgeError, "transient transport failure") {
		t.Errorf("JudgeError = %q, should contain stub message", fact.JudgeError)
	}
	if fact.JudgePromptSHA != "abc123" || fact.JudgeLatencyMS != 31 {
		t.Errorf("forensic fields not propagated: prompt_sha=%q latency=%d", fact.JudgePromptSHA, fact.JudgeLatencyMS)
	}
}

// TestScriptSessionEvaluator_URLUnrecognized_JudgeBlock_EmptyGuidance
// confirms the evaluator substitutes a generic fallback guidance when
// the judge blocks without text — the agent still gets something
// actionable rather than an empty refusal.
func TestScriptSessionEvaluator_URLUnrecognized_JudgeBlock_EmptyGuidance(t *testing.T) {
	judge := &stubJudge{verdict: scriptjudge.Verdict{Allow: false, Reason: "no http request"}}
	e := policies.NewScriptSessionEvaluator(func(_ context.Context, _ conversation.ToolUse) *policies.ScriptSessionInputs {
		return &policies.ScriptSessionInputs{ResolverBaseURL: "http://localhost:25297/api/proxy", Judge: judge}
	})
	v, err := e.Evaluate(context.Background(), newStubResp(), urlUnrecognizedToolUse(), &recordingMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeDeny {
		t.Errorf("Outcome = %q, want Deny", v.Outcome)
	}
	if !strings.Contains(v.Reason, "doesn't appear to target the resolver") {
		t.Errorf("Reason %q should carry generic fallback guidance when AgentGuidance is empty", v.Reason)
	}
}
