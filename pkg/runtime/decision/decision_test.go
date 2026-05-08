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
