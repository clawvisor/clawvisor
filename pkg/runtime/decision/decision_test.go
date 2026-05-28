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

func TestEvaluateAuthorization_HardDenyMatchesCrossHarnessToolAlias(t *testing.T) {
	agentID := "agent-1"
	toolDeny := rule("tool-deny", "tool", "deny", &agentID)
	toolDeny.ToolName = "Bash"
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
		t.Fatalf("decision = %+v, want Bash hard-deny to block exec_command alias", got)
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

func TestEvaluateAuthorization_ToolAllowOverridesTaskIntentRefusal(t *testing.T) {
	agentID := "agent-1"
	allow := rule("allow", "tool", "allow", &agentID)
	allow.ToolName = "Read"
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: false, Explanation: "file outside task scope"}}

	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("Read", map[string]any{"file_path": "/tmp/blah1/hello.go"}),
		AgentID:        agentID,
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", agentID, "Read", "read files in /tmp/blah2")},
		ToolRules:      []*store.RuntimePolicyRule{allow},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceRuleAllow || got.Rule != allow {
		t.Fatalf("decision = %+v, want immediate rule allow", got)
	}
	if verifier.called {
		t.Fatal("intent verifier should not run after always-allow tool rule")
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

func TestEvaluateAuthorization_PreferredTaskDisambiguatesToolScope(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true, Explanation: "fits checked-out task"}}
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:         toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:         "agent-1",
		CandidateTasks:  []*store.Task{taskWithExpectedTool("task-1", "agent-1", "exec_command", "read repo files"), taskWithExpectedTool("task-2", "agent-1", "exec_command", "read repo files")},
		PreferredTaskID: "task-2",
		IntentVerifier:  verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope || got.Task == nil || got.Task.ID != "task-2" {
		t.Fatalf("decision = %+v, want preferred task-scope allow for task-2", got)
	}
	if verifier.last.TaskID != "task-2" {
		t.Fatalf("intent verifier TaskID = %q, want task-2", verifier.last.TaskID)
	}
}

func TestEvaluateAuthorization_PreferredTaskDisambiguatesServiceAction(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true, Explanation: "fits checked-out task"}}
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:         toolUse("WebFetch", map[string]any{"url": "https://api.github.com/repos/acme/app/issues"}),
		AgentID:         "agent-1",
		Service:         "github",
		Action:          "create_issue",
		CandidateTasks:  []*store.Task{taskWithAction("task-1", "agent-1", "github", "create_issue", "strict"), taskWithAction("task-2", "agent-1", "github", "create_issue", "strict")},
		PreferredTaskID: "task-2",
		IntentVerifier:  verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope || got.Task == nil || got.Task.ID != "task-2" {
		t.Fatalf("decision = %+v, want preferred task-scope allow for task-2", got)
	}
	if verifier.last.TaskID != "task-2" {
		t.Fatalf("intent verifier TaskID = %q, want task-2", verifier.last.TaskID)
	}
}

func TestEvaluateAuthorization_IgnoresPreferredTaskOutsideCandidateSet(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:         toolUse("WebFetch", map[string]any{"url": "https://api.github.com/repos/acme/app/issues"}),
		AgentID:         "agent-1",
		Service:         "github",
		Action:          "create_issue",
		CandidateTasks:  []*store.Task{taskWithAction("task-1", "agent-1", "github", "create_issue", "strict"), taskWithAction("task-2", "agent-1", "github", "create_issue", "strict")},
		PreferredTaskID: "task-other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictNeedsApproval || got.Source != SourceTaskScopeAmbiguous {
		t.Fatalf("decision = %+v, want ambiguity when preferred task is not a valid candidate", got)
	}
}

func TestEvaluateAuthorization_CanSkipIntentForLocallyClassifiedLowRiskTool(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: false, Explanation: "would block if called"}}
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:                toolUse("exec_command", map[string]any{"cmd": "cat README.md"}),
		AgentID:                "agent-1",
		CandidateTasks:         []*store.Task{taskWithExpectedTool("task-1", "agent-1", "exec_command", "read repo files")},
		IntentVerifier:         verifier,
		SkipIntentVerification: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow || got.Source != SourceTaskScope {
		t.Fatalf("decision = %+v, want task-scope allow", got)
	}
	if verifier.called {
		t.Fatal("intent verifier should not run when SkipIntentVerification is set")
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
// `expected_tools` before defaulting to approval-required. The
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
// in expected_tools; the same task should cover the equivalent
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

// Regression: OpenClaw exposes its shell surface as `exec`, while the
// task prompt asks models to declare shell scope as `Bash`. An inline
// approved task with expected_tools[0].tool_name="Bash" must cover
// a later OpenClaw `exec` call, otherwise the user sees a second
// no-matching-task-scope approval immediately after approving the task.
func TestEvaluateAuthorization_ExpectedToolMatchAcceptsOpenClawExecAlias(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec", map[string]any{"command": "openclaw cron --help 2>&1 | head -60"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect openclaw cron help")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VerdictAllow {
		t.Fatalf("decision = %+v, want VerdictAllow (Bash should cover OpenClaw exec)", got)
	}
}

// Regression: models populate expected_tools from documentation
// and examples; they routinely use lowercase tool names (`bash`) even
// when the harness reports `Bash`. The task-scope matcher must be
// case-insensitive so the model-emitted lowercase form covers the
// harness's capitalized actual tool name.
func TestEvaluateAuthorization_ExpectedToolMatchIsCaseInsensitive(t *testing.T) {
	got, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", map[string]any{"command": "curl https://api.github.com/user"}),
		AgentID: "agent-1",
		Service: "github",
		Action:  "get_user",
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

// Claude Code's Bash `description` is a label of WHAT the command
// does, not WHY — folding it in as the per-call Reason flagged
// benign commands (e.g. "List llmproxy directory") as
// reason_coherence: insufficient. Bash now falls through to the
// sentinel path so the verifier deduces intent from params + task
// purpose. A genuine `reason` or `rationale` field is still forwarded.
func TestEvaluateAuthorization_BashDescriptionIsNotForwardedAsReason(t *testing.T) {
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
	if verifier.last.Reason != NoPerCallReasonSentinel {
		t.Fatalf("Reason = %q, want NoPerCallReasonSentinel (Bash description must not be treated as a rationale)", verifier.last.Reason)
	}
}

// A genuine `reason` field in tool input is still forwarded as the
// per-call rationale (only `description` is excluded).
func TestEvaluateAuthorization_ToolReasonForwardsExplicitReasonField(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", map[string]any{
			"command": "ls /tmp",
			"reason":  "confirming the staged files landed before zipping",
		}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.last.Reason != "confirming the staged files landed before zipping" {
		t.Fatalf("Reason = %q, want explicit reason field forwarded", verifier.last.Reason)
	}
}

// Defense-in-depth: if the model sets the sentinel in one rationale
// field and a plausible string in another, the bypass attempt is the
// signal — all rationale fields are poisoned and we fall through.
func TestEvaluateAuthorization_ToolReasonSentinelInOneFieldPoisonsOthers(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse: toolUse("Bash", map[string]any{
			"command":   "ls /tmp",
			"reason":    NoPerCallReasonSentinel,
			"rationale": "totally legitimate-sounding string",
		}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "Bash", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.last.Reason == "totally legitimate-sounding string" {
		t.Fatalf("Reason surfaced a different field after a sentinel bypass attempt: %q", verifier.last.Reason)
	}
}

// Codex's shell tool genuinely doesn't have a rationale field in its
// schema (argv only). For these, sending NoPerCallReasonSentinel is
// correct — the verifier prompt knows to skip coherence rather than
// flag insufficient on the harness's non-fault.
func TestEvaluateAuthorization_GenuineNoRationaleHarnessUsesSentinel(t *testing.T) {
	verifier := &stubIntentVerifier{verdict: &IntentVerdict{Allow: true}}
	_, err := EvaluateAuthorization(context.Background(), AuthorizationInput{
		ToolUse:        toolUse("exec_command", map[string]any{"cmd": "ls /tmp"}),
		AgentID:        "agent-1",
		CandidateTasks: []*store.Task{taskWithExpectedTool("task-1", "agent-1", "exec_command", "inspect filesystem")},
		IntentVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifier.last.Reason != NoPerCallReasonSentinel {
		t.Fatalf("Reason = %q, want NoPerCallReasonSentinel for argv-only Codex shell", verifier.last.Reason)
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
