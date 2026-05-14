package decision

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type stubIntentVerifier struct {
	verdict *IntentVerdict
	err     error
	called  bool
	last    IntentVerifyRequest
}

func (s *stubIntentVerifier) Verify(_ context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	s.called = true
	s.last = req
	return s.verdict, s.err
}

func TestEvaluateAuthorization_EgressDenyOverridesToolAllow(t *testing.T) {
	agentID := "agent-1"
	toolAllow := rule("tool-allow", "tool", "allow", &agentID)
	toolAllow.ToolName = "Bash"
	egressDeny := rule("egress-deny", "egress", "deny", &agentID)
	egressDeny.Host = "api.github.com"
	egressDeny.Method = "POST"
	egressDeny.Path = "/repos/acme/app/issues"

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:     toolUse("Bash", map[string]any{"cmd": "curl"}),
		AgentID:     agentID,
		Target:      TargetRequest{Host: "api.github.com", Method: "POST", Path: "/repos/acme/app/issues"},
		ToolRules:   []*store.RuntimePolicyRule{toolAllow},
		EgressRules: []*store.RuntimePolicyRule{egressDeny},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictDeny || got.Source != SourceRuleDeny || got.Rule != egressDeny {
		t.Fatalf("decision = %+v, want egress deny", got)
	}
}

func TestEvaluateAuthorization_ToolReviewOverridesEgressAllow(t *testing.T) {
	agentID := "agent-1"
	toolReview := rule("tool-review", "tool", "review", &agentID)
	toolReview.ToolName = "Bash"
	egressAllow := rule("egress-allow", "egress", "allow", &agentID)
	egressAllow.Host = "api.github.com"

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:     toolUse("Bash", nil),
		AgentID:     agentID,
		Target:      TargetRequest{Host: "api.github.com", Method: "GET", Path: "/"},
		ToolRules:   []*store.RuntimePolicyRule{toolReview},
		EgressRules: []*store.RuntimePolicyRule{egressAllow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictNeedsApproval || got.Source != SourceRuleReview || got.Rule != toolReview {
		t.Fatalf("decision = %+v, want tool review", got)
	}
}

func TestEvaluateAuthorization_TaskScopeOverridesToolReview(t *testing.T) {
	agentID := "agent-1"
	toolReview := rule("tool-review", "tool", "review", &agentID)
	toolReview.ToolName = "exec_command"
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true, Explanation: "fits task"}}

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:        agentID,
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", agentID, "exec_command", "read repo files")},
		ToolRules:      []*store.RuntimePolicyRule{toolReview},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope || got.Rule != nil || got.Task == nil || got.Task.ID != "task-1" {
		t.Fatalf("decision = %+v, want task-scope allow", got)
	}
	if !verifier.called {
		t.Fatal("expected intent verifier to be called")
	}
}

func TestEvaluateAuthorization_HardDenyOverridesTaskScope(t *testing.T) {
	agentID := "agent-1"
	toolDeny := rule("tool-deny", "tool", "deny", &agentID)
	toolDeny.ToolName = "exec_command"
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true, Explanation: "fits task"}}

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:        agentID,
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", agentID, "exec_command", "read repo files")},
		ToolRules:      []*store.RuntimePolicyRule{toolDeny},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictDeny || got.Source != SourceRuleDeny || got.Rule != toolDeny {
		t.Fatalf("decision = %+v, want hard deny", got)
	}
	if verifier.called {
		t.Fatal("intent verifier should not run after hard deny")
	}
}

func TestEvaluateAuthorization_ObserveDoesNotSoftenIntentRefusal(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: false, Explanation: "wrong repo"}}
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Bash", map[string]any{"repo": "other"}),
		AgentID:        "agent-1",
		Posture:        PostureObserve,
		Service:        "github",
		Action:         "create_issue",
		CandidateTasks: []*store.Task{taskWithAction("task-1", "agent-1", "github", "create_issue", "strict")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictDeny || got.Source != SourceIntentRefusal || got.DenyReason != DenyReasonIntent {
		t.Fatalf("decision = %+v, want intent deny even in observe", got)
	}
	if !verifier.called {
		t.Fatal("expected intent verifier to be called")
	}
}

func TestEvaluateAuthorization_RuleAllowOverridesMissingTaskScope(t *testing.T) {
	agentID := "agent-1"
	allow := rule("allow", "tool", "allow", &agentID)
	allow.ToolName = "Bash"

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Bash", nil),
		AgentID:        agentID,
		Service:        "github",
		Action:         "delete_repo",
		CandidateTasks: []*store.Task{taskWithAction("task-1", agentID, "github", "create_issue", "off")},
		ToolRules:      []*store.RuntimePolicyRule{allow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceRuleAllow {
		t.Fatalf("decision = %+v, want rule allow", got)
	}
}

func TestEvaluateAuthorization_AmbiguousScopeNeedsApproval(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", nil),
		AgentID: "agent-1",
		Service: "github",
		Action:  "create_issue",
		CandidateTasks: []*store.Task{
			taskWithAction("task-1", "agent-1", "github", "create_issue", "off"),
			taskWithAction("task-2", "agent-1", "github", "create_issue", "off"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictNeedsApproval || got.Source != SourceTaskScopeAmbiguous {
		t.Fatalf("decision = %+v, want ambiguous review", got)
	}
}

func TestEvaluateAuthorization_EmptyPostureDefaultsToEnforce(t *testing.T) {
	agentID := "agent-1"
	deny := rule("deny", "tool", "deny", &agentID)
	deny.ToolName = "Bash"

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:   toolUse("Bash", nil),
		AgentID:   agentID,
		ToolRules: []*store.RuntimePolicyRule{deny},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictDeny || got.ObservationEffect != ObservationNone {
		t.Fatalf("decision = %+v, want enforce deny", got)
	}
}

func TestEvaluateAuthorization_NilIntentVerifierSkipsIntent(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Bash", nil),
		AgentID:        "agent-1",
		Service:        "github",
		Action:         "create_issue",
		CandidateTasks: []*store.Task{taskWithAction("task-1", "agent-1", "github", "create_issue", "strict")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope {
		t.Fatalf("decision = %+v, want task-scope allow", got)
	}
}

func TestEvaluateAuthorization_ToolTaskRunsIntentVerifier(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true, Explanation: "fits task"}}
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "exec_command", "read repo files")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope || got.Task == nil || got.Task.ID != "task-1" {
		t.Fatalf("decision = %+v, want task-scope allow", got)
	}
	if !verifier.called {
		t.Fatal("expected intent verifier to be called")
	}
	if verifier.last.Service != "runtime.tool" || verifier.last.Action != "exec_command" {
		t.Fatalf("intent request service/action = %s/%s", verifier.last.Service, verifier.last.Action)
	}
	if verifier.last.ExpectedUse != "inspect files only" {
		t.Fatalf("intent request ExpectedUse = %q", verifier.last.ExpectedUse)
	}
	if verifier.last.TaskID != "task-1" {
		t.Fatalf("intent request TaskID = %q", verifier.last.TaskID)
	}
}

func TestEvaluateAuthorization_ToolTaskIntentRefusalNeedsApproval(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: false, Explanation: "write command outside scope"}}
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "rm README.md"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "exec_command", "read repo files")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictNeedsApproval || got.Source != SourceIntentRefusal || got.DenyReason != DenyReasonIntent {
		t.Fatalf("decision = %+v, want intent refusal review", got)
	}
	if got.Reason != "write command outside scope" {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestEvaluateAuthorization_ToolTaskIntentOffSkipsVerifier(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: false, Explanation: "should not be called"}}
	task := taskWithExpectedTool("task-1", "agent-1", "exec_command", "read repo files")
	task.IntentVerificationMode = "off"
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{task},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.called {
		t.Fatal("intent verifier should not be called when mode is off")
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope {
		t.Fatalf("decision = %+v, want task-scope allow", got)
	}
}

func TestEvaluateAuthorization_ToolTaskIntentLenientFlag(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	task := taskWithExpectedTool("task-1", "agent-1", "exec_command", "read repo files")
	task.IntentVerificationMode = "lenient"
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{task},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.called || !verifier.last.Lenient {
		t.Fatalf("expected lenient verifier request, got called=%v last=%+v", verifier.called, verifier.last)
	}
}

// Regression: when the lite-proxy's catalog resolves a credentialed
// call to (service, action) but no task declared `authorized_actions`
// for it, the evaluator should still try matching against the task's
// `expected_tools_json` before defaulting to approval-required. The
// lite-proxy's taskCreationPrompt steers the model to declare scope
// via expected_tools, so a previously-approved task that covers
// (Bash, "curl api.github.com/user") must allow the credentialed call
// without a second inline approval.
func TestEvaluateAuthorization_ServiceActionFallsBackToExpectedTool(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Bash", map[string]any{"command": "curl https://api.github.com/user"}),
		AgentID:        "agent-1",
		Service:        "github",
		Action:         "get_user",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "fetch GitHub user info")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow {
		t.Fatalf("decision = %+v, want VerdictAllow", got)
	}
	if got.Source != SourceTaskScope {
		t.Fatalf("source = %s, want SourceTaskScope", got.Source)
	}
	if got.Task == nil || got.Task.ID != "task-1" {
		t.Fatalf("expected to match task-1, got task=%+v", got.Task)
	}
}

// Regression: a task created in a Claude Code session declares Bash
// in expected_tools_json; the same task should cover the equivalent
// work when the user is in a Codex session that emits `exec_command`.
// Cross-harness tool aliases must resolve through the toolClass map.
func TestEvaluateAuthorization_ExpectedToolMatchAcceptsCrossHarnessAliases(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "mkdir -p /tmp/landing"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "scaffold the landing page")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow {
		t.Fatalf("decision = %+v, want VerdictAllow (Bash should cover exec_command)", got)
	}
}

// Regression: models populate expected_tools_json from documentation
// and examples; they routinely use lowercase tool names (`bash`) even
// when the harness reports `Bash`. The task-scope matcher must be
// case-insensitive so the model-emitted lowercase form covers the
// harness's capitalized actual tool name.
func TestEvaluateAuthorization_ExpectedToolMatchIsCaseInsensitive(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Bash", map[string]any{"command": "curl https://api.github.com/user"}),
		AgentID:        "agent-1",
		Service:        "github",
		Action:         "get_user",
		// Task declared lowercase "bash" (what the model wrote).
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "bash", "fetch user")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow {
		t.Fatalf("decision = %+v, want VerdictAllow (case-insensitive match)", got)
	}
}

// The per-call Reason must come from tool input (Claude Code's Bash
// supplies a short `description` on every call), not from the task's
// expected_tools[].why — that scope text is what we already pass as
// ExpectedUse, and the verifier correctly flags a verbatim copy as
// "procedural instructions rather than a 'why' clause." See
// internal/intent/prompts.go for the verifier's reason_coherence rules.
func TestEvaluateAuthorization_ToolReasonPrefersToolInputDescription(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", map[string]any{
			"command":     "ls /tmp",
			"description": "List temp directory contents",
		}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.last.Reason != "List temp directory contents" {
		t.Fatalf("Reason = %q, want tool input description", verifier.last.Reason)
	}
	if verifier.last.Reason == verifier.last.ExpectedUse {
		t.Fatalf("Reason must not be a verbatim copy of ExpectedUse: %q", verifier.last.Reason)
	}
}

// A malicious model can set its tool input's `description` to the
// exact NoPerCallReasonSentinel string, which the verifier prompt
// treats as "harness doesn't collect a rationale" and uses to skip
// the reason_coherence check. Forwarding that verbatim would let the
// model disable a defense layer per call. The proxy must refuse to
// honor a model-supplied value equal to the sentinel.
func TestEvaluateAuthorization_ToolReasonRefusesModelSuppliedSentinel(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", map[string]any{
			"command":     "ls /tmp",
			"description": NoPerCallReasonSentinel,
		}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	// We DO send the sentinel as the system-supplied fallback, so the
	// Reason field on the wire equals the sentinel. The distinction we
	// care about: it came from OUR fallback path, not from a model
	// string that exactly equaled the sentinel. That distinction is
	// preserved by perCallToolReason refusing the match before
	// firstNonEmpty considers it. The observable signal: Reason
	// equals the sentinel even though the model TRIED to supply it,
	// which only happens via the fallback branch (i.e. the model's
	// string was rejected and we fell through).
	if verifier.last.Reason != NoPerCallReasonSentinel {
		t.Fatalf("Reason = %q; want sentinel (model-supplied value should have been rejected and fallback used)", verifier.last.Reason)
	}
}

// Defense-in-depth: if the model sets the sentinel in ANY rationale
// field, fall through to the system sentinel — don't let the model
// surface a different field as the rationale by knowing the lookup
// order. A bypass attempt in one field poisons all fields.
func TestEvaluateAuthorization_ToolReasonSentinelInOneFieldPoisonsOthers(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", map[string]any{
			"command":     "ls /tmp",
			"description": NoPerCallReasonSentinel,
			"reason":      "totally legitimate-sounding string",
		}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.last.Reason != NoPerCallReasonSentinel {
		t.Fatalf("Reason = %q; want system sentinel (model bypass attempt should have poisoned all fields)", verifier.last.Reason)
	}
}

// When the harness doesn't supply a per-call rationale (Codex's shell
// sends argv only, no description), we must send the
// NoPerCallReasonSentinel so the verifier prompt knows to skip the
// reason_coherence check rather than flag it as "insufficient." The
// fallback must NOT be the task's expected_tools[].why (verbatim-copy
// refusal) and must NOT be a bare action name like "tool_use Bash"
// (would trip insufficient).
func TestEvaluateAuthorization_ToolReasonFallbackUsesSentinel(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Bash", map[string]any{"command": "ls /tmp"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.last.Reason != NoPerCallReasonSentinel {
		t.Fatalf("Reason = %q, want NoPerCallReasonSentinel", verifier.last.Reason)
	}
}

func toolUse(name string, input map[string]any) conversation.ToolUse {
	raw, _ := json.Marshal(input)
	return conversation.ToolUse{ID: "toolu_1", Name: name, Input: raw}
}

func rule(id, kind, action string, agentID *string) *store.RuntimePolicyRule {
	return &store.RuntimePolicyRule{
		ID:      id,
		AgentID: agentID,
		Kind:    kind,
		Action:  action,
		Enabled: true,
	}
}

func taskWithAction(id, agentID, service, action, verification string) *store.Task {
	return &store.Task{
		ID:      id,
		AgentID: agentID,
		Status:  "active",
		AuthorizedActions: []store.TaskAction{{
			Service:      service,
			Action:       action,
			ExpectedUse:  "expected use",
			Verification: verification,
		}},
	}
}

func taskWithExpectedTool(id, agentID, toolName, why string) *store.Task {
	return &store.Task{
		ID:                     id,
		AgentID:                agentID,
		Purpose:                "Inspect repository files",
		Status:                 "active",
		IntentVerificationMode: "strict",
		ExpectedUse:            "inspect files only",
		ExpectedTools:          json.RawMessage(`[{"tool_name":"` + toolName + `","why":"` + why + `"}]`),
	}
}
