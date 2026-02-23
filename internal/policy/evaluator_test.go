package policy

import (
	"testing"
	"time"
)

// boolPtr is a helper for *bool fields.
func boolPtr(b bool) *bool { return &b }

// makeRule builds a CompiledRule for testing.
func makeRule(id, service string, actions []string, decision Decision, condition *Condition, priority int) CompiledRule {
	return CompiledRule{
		ID:       id,
		PolicyID: "test-policy",
		Service:  service,
		Actions:  actions,
		Decision: decision,
		Priority: priority,
	}
}

func makeRuleWithCondition(id, service string, actions []string, decision Decision, cond *Condition, priority int) CompiledRule {
	r := makeRule(id, service, actions, decision, cond, priority)
	r.Condition = cond
	return r
}

// ── Basic decision tests ──────────────────────────────────────────────────────

func TestEmptyRules_DefaultApprove(t *testing.T) {
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, nil)
	if d.Decision != DecisionApprove {
		t.Errorf("expected approve, got %s", d.Decision)
	}
}

func TestSingleExecuteRule(t *testing.T) {
	rules := []CompiledRule{
		makeRule("r1", "google.gmail", []string{"list_messages"}, DecisionExecute, nil, 10),
	}
	req := EvalRequest{Service: "google.gmail", Action: "list_messages"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionExecute {
		t.Errorf("expected execute, got %s", d.Decision)
	}
}

func TestSingleBlockRule(t *testing.T) {
	rules := []CompiledRule{
		makeRule("r1", "google.gmail", []string{"send_message"}, DecisionBlock, nil, 100),
	}
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionBlock {
		t.Errorf("expected block, got %s", d.Decision)
	}
}

func TestBlockBeatsApprove(t *testing.T) {
	rules := []CompiledRule{
		makeRule("block", "google.gmail", []string{"send_message"}, DecisionBlock, nil, 100),
		makeRule("approve", "google.gmail", []string{"send_message"}, DecisionApprove, nil, 50),
	}
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionBlock {
		t.Errorf("block should beat approve; got %s", d.Decision)
	}
}

func TestBlockBeatsExecute(t *testing.T) {
	rules := []CompiledRule{
		makeRule("block", "google.gmail", []string{"send_message"}, DecisionBlock, nil, 100),
		makeRule("execute", "google.gmail", []string{"send_message"}, DecisionExecute, nil, 10),
	}
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionBlock {
		t.Errorf("block should beat execute; got %s", d.Decision)
	}
}

func TestApproveBeatsExecute(t *testing.T) {
	rules := []CompiledRule{
		makeRule("approve", "google.gmail", []string{"*"}, DecisionApprove, nil, 50),
		makeRule("execute", "google.gmail", []string{"list_messages"}, DecisionExecute, nil, 20),
	}
	req := EvalRequest{Service: "google.gmail", Action: "list_messages"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionApprove {
		t.Errorf("approve should beat execute; got %s", d.Decision)
	}
}

// ── No match ──────────────────────────────────────────────────────────────────

func TestNoMatchingRule_DefaultApprove(t *testing.T) {
	rules := []CompiledRule{
		makeRule("r1", "google.gmail", []string{"list_messages"}, DecisionExecute, nil, 10),
	}
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionApprove {
		t.Errorf("no match should default to approve; got %s", d.Decision)
	}
}

// ── Wildcard ──────────────────────────────────────────────────────────────────

func TestWildcardAction_MatchesAny(t *testing.T) {
	rules := []CompiledRule{
		makeRule("r1", "google.gmail", []string{"*"}, DecisionExecute, nil, 10),
	}
	for _, action := range []string{"list_messages", "send_message", "delete_message"} {
		req := EvalRequest{Service: "google.gmail", Action: action}
		d := Evaluate(req, rules)
		if d.Decision != DecisionExecute {
			t.Errorf("wildcard action should match %q; got %s", action, d.Decision)
		}
	}
}

func TestWildcardService_MatchesAny(t *testing.T) {
	rules := []CompiledRule{
		makeRule("r1", "*", []string{"read"}, DecisionExecute, nil, 10),
	}
	for _, svc := range []string{"google.gmail", "github", "google.drive"} {
		req := EvalRequest{Service: svc, Action: "read"}
		d := Evaluate(req, rules)
		if d.Decision != DecisionExecute {
			t.Errorf("wildcard service should match %q; got %s", svc, d.Decision)
		}
	}
}

func TestSpecificAction_NoMatch(t *testing.T) {
	rules := []CompiledRule{
		makeRule("r1", "google.gmail", []string{"list_messages"}, DecisionBlock, nil, 100),
	}
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionApprove {
		t.Errorf("specific action should not match different action; got %s", d.Decision)
	}
}

// ── Conditions ────────────────────────────────────────────────────────────────

func TestCondition_ParamMatches_Fires(t *testing.T) {
	cond := &Condition{Type: "param_matches", Param: "to", Pattern: `^.+@mycompany\.com$`}
	rules := []CompiledRule{
		makeRuleWithCondition("r1", "google.gmail", []string{"send_message"}, DecisionExecute, cond, 30),
	}
	req := EvalRequest{
		Service: "google.gmail",
		Action:  "send_message",
		Params:  map[string]any{"to": "alice@mycompany.com"},
	}
	d := Evaluate(req, rules)
	if d.Decision != DecisionExecute {
		t.Errorf("condition should fire; got %s", d.Decision)
	}
}

func TestCondition_ParamMatches_NotFires(t *testing.T) {
	cond := &Condition{Type: "param_matches", Param: "to", Pattern: `^.+@mycompany\.com$`}
	rules := []CompiledRule{
		makeRuleWithCondition("r1", "google.gmail", []string{"send_message"}, DecisionExecute, cond, 30),
	}
	req := EvalRequest{
		Service: "google.gmail",
		Action:  "send_message",
		Params:  map[string]any{"to": "external@example.com"},
	}
	d := Evaluate(req, rules)
	if d.Decision != DecisionApprove {
		t.Errorf("condition should not fire for non-matching param; got %s", d.Decision)
	}
}

func TestCondition_ParamNotContains(t *testing.T) {
	cond := &Condition{Type: "param_not_contains", Param: "subject", Value: "urgent"}
	rules := []CompiledRule{
		makeRuleWithCondition("r1", "google.gmail", []string{"send_message"}, DecisionExecute, cond, 30),
	}

	// Should fire (param does not contain "urgent")
	req := EvalRequest{
		Service: "google.gmail", Action: "send_message",
		Params: map[string]any{"subject": "hello world"},
	}
	if d := Evaluate(req, rules); d.Decision != DecisionExecute {
		t.Errorf("param_not_contains: should fire when value absent; got %s", d.Decision)
	}

	// Should not fire (param contains "urgent")
	req.Params = map[string]any{"subject": "urgent: meeting"}
	if d := Evaluate(req, rules); d.Decision != DecisionApprove {
		t.Errorf("param_not_contains: should not fire when value present; got %s", d.Decision)
	}
}

func TestCondition_MaxResultsUnder(t *testing.T) {
	cond := &Condition{Type: "max_results_under", Param: "limit", Max: 100}
	rules := []CompiledRule{
		makeRuleWithCondition("r1", "*", []string{"*"}, DecisionExecute, cond, 30),
	}

	// Under max → fires
	req := EvalRequest{Service: "github", Action: "list_repos", Params: map[string]any{"limit": float64(50)}}
	if d := Evaluate(req, rules); d.Decision != DecisionExecute {
		t.Errorf("max_results_under: should fire when under; got %s", d.Decision)
	}

	// At max → doesn't fire
	req.Params = map[string]any{"limit": float64(100)}
	if d := Evaluate(req, rules); d.Decision != DecisionApprove {
		t.Errorf("max_results_under: should not fire when at max; got %s", d.Decision)
	}
}

func TestCondition_RecipientInContacts_Resolved(t *testing.T) {
	cond := &Condition{Type: "recipient_in_contacts"}
	rules := []CompiledRule{
		makeRuleWithCondition("r1", "google.gmail", []string{"send_message"}, DecisionExecute, cond, 30),
	}

	// Resolved true → fires
	req := EvalRequest{
		Service:            "google.gmail",
		Action:             "send_message",
		ResolvedConditions: map[string]bool{"recipient_in_contacts": true},
	}
	if d := Evaluate(req, rules); d.Decision != DecisionExecute {
		t.Errorf("recipient_in_contacts resolved true: should fire; got %s", d.Decision)
	}

	// Resolved false → doesn't fire
	req.ResolvedConditions = map[string]bool{"recipient_in_contacts": false}
	if d := Evaluate(req, rules); d.Decision != DecisionApprove {
		t.Errorf("recipient_in_contacts resolved false: should not fire; got %s", d.Decision)
	}

	// Not in map (contacts not activated) → doesn't fire
	req.ResolvedConditions = nil
	if d := Evaluate(req, rules); d.Decision != DecisionApprove {
		t.Errorf("recipient_in_contacts absent: should not fire; got %s", d.Decision)
	}
}

// ── Time window ───────────────────────────────────────────────────────────────

func TestTimeWindow_InsideWindow(t *testing.T) {
	// Monday 10:00 UTC
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC) // Monday
	tw := &TimeWindow{Days: []string{"mon"}, Hours: "08:00-18:00", Timezone: "UTC"}
	if !matchesTimeWindow(tw, now) {
		t.Error("should be inside time window")
	}
}

func TestTimeWindow_OutsideHours(t *testing.T) {
	now := time.Date(2024, 1, 15, 20, 0, 0, 0, time.UTC) // Monday 20:00
	tw := &TimeWindow{Days: []string{"mon"}, Hours: "08:00-18:00", Timezone: "UTC"}
	if matchesTimeWindow(tw, now) {
		t.Error("should be outside time window (after hours)")
	}
}

func TestTimeWindow_OutsideDay(t *testing.T) {
	now := time.Date(2024, 1, 14, 10, 0, 0, 0, time.UTC) // Sunday
	tw := &TimeWindow{Days: []string{"mon", "tue", "wed", "thu", "fri"}, Hours: "08:00-18:00", Timezone: "UTC"}
	if matchesTimeWindow(tw, now) {
		t.Error("should be outside time window (weekend)")
	}
}

// ── Role precedence ───────────────────────────────────────────────────────────

func TestRoleRulePrecedence_OverridesGlobal(t *testing.T) {
	// Global: block send_message
	// Role "researcher": execute send_message
	global := CompiledRule{
		ID: "global:block", PolicyID: "global", RoleID: "",
		Service: "google.gmail", Actions: []string{"send_message"},
		Decision: DecisionBlock, Priority: 100,
	}
	roleRule := CompiledRule{
		ID: "researcher:execute", PolicyID: "researcher-policy", RoleID: "researcher",
		Service: "google.gmail", Actions: []string{"send_message"},
		Decision: DecisionExecute, Priority: 10,
	}
	rules := []CompiledRule{global, roleRule}

	req := EvalRequest{Service: "google.gmail", Action: "send_message", AgentRoleID: "researcher"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionExecute {
		t.Errorf("role rule should override global; got %s", d.Decision)
	}
}

func TestNoRole_GlobalOnly(t *testing.T) {
	roleRule := CompiledRule{
		ID: "researcher:execute", PolicyID: "researcher-policy", RoleID: "researcher",
		Service: "google.gmail", Actions: []string{"send_message"},
		Decision: DecisionExecute, Priority: 10,
	}
	rules := []CompiledRule{roleRule}

	req := EvalRequest{Service: "google.gmail", Action: "send_message", AgentRoleID: ""}
	d := Evaluate(req, rules)
	// No global rules match, no role matches → default approve
	if d.Decision != DecisionApprove {
		t.Errorf("agent with no role should not get role-specific rules; got %s", d.Decision)
	}
}

func TestGlobalPolicyAppliesWithRole(t *testing.T) {
	globalBlock := CompiledRule{
		ID: "global:block", PolicyID: "global", RoleID: "",
		Service: "google.gmail", Actions: []string{"delete_message"},
		Decision: DecisionBlock, Priority: 100,
	}
	rules := []CompiledRule{globalBlock}

	// Agent with role "researcher" — global block should still apply to uncovered actions
	req := EvalRequest{Service: "google.gmail", Action: "delete_message", AgentRoleID: "researcher"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionBlock {
		t.Errorf("global policy should apply even to agents with roles; got %s", d.Decision)
	}
}

// ── require_approval (compiles to DecisionApprove) ───────────────────────────

func TestRequireApprovalCompilesToApprove(t *testing.T) {
	allow := true
	rule := Rule{
		Service: "google.gmail", Actions: []string{"send_message"},
		Allow: &allow, RequireApproval: true,
	}
	policy := Policy{ID: "p1", Rules: []Rule{rule}}
	compiled := Compile([]Policy{policy}, "user1")
	if len(compiled) != 1 {
		t.Fatalf("expected 1 compiled rule, got %d", len(compiled))
	}
	if compiled[0].Decision != DecisionApprove {
		t.Errorf("require_approval should compile to approve; got %s", compiled[0].Decision)
	}
}

// ── Priority ordering ─────────────────────────────────────────────────────────

func TestCompiler_PriorityOrder(t *testing.T) {
	allow := true
	deny := false
	policies := []Policy{{
		ID: "p1",
		Rules: []Rule{
			{Service: "google.gmail", Actions: []string{"*"}, Allow: &allow},      // execute, wildcard → low priority
			{Service: "google.gmail", Actions: []string{"send_message"}, Allow: &deny}, // block, specific → high priority
		},
	}}
	compiled := Compile(policies, "user1")
	if len(compiled) != 2 {
		t.Fatalf("expected 2 compiled rules, got %d", len(compiled))
	}
	if compiled[0].Decision != DecisionBlock {
		t.Errorf("block rule should be highest priority after sort; got %s", compiled[0].Decision)
	}
}

// ── ResponseFilters ───────────────────────────────────────────────────────────

func TestResponseFilters_CollectedFromExecuteRules(t *testing.T) {
	filter := ResponseFilter{Redact: "email_address"}
	rules := []CompiledRule{
		{
			ID: "r1", PolicyID: "p1", Service: "google.gmail",
			Actions: []string{"list_messages"}, Decision: DecisionExecute,
			Priority: 10, ResponseFilters: []ResponseFilter{filter},
		},
	}
	req := EvalRequest{Service: "google.gmail", Action: "list_messages"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionExecute {
		t.Fatalf("expected execute; got %s", d.Decision)
	}
	if len(d.ResponseFilters) != 1 || d.ResponseFilters[0].Redact != "email_address" {
		t.Errorf("response filters not collected; got %+v", d.ResponseFilters)
	}
}

// ── Compiler: multiple block rules ───────────────────────────────────────────

func TestMultipleBlockRules_FirstWins(t *testing.T) {
	rules := []CompiledRule{
		{ID: "r1", PolicyID: "p1", Service: "google.gmail", Actions: []string{"send_message"}, Decision: DecisionBlock, Priority: 100, Reason: "first"},
		{ID: "r2", PolicyID: "p1", Service: "google.gmail", Actions: []string{"send_message"}, Decision: DecisionBlock, Priority: 100, Reason: "second"},
	}
	req := EvalRequest{Service: "google.gmail", Action: "send_message"}
	d := Evaluate(req, rules)
	if d.Decision != DecisionBlock {
		t.Fatalf("expected block; got %s", d.Decision)
	}
	if d.RuleID != "r1" {
		t.Errorf("first block rule should win; got ruleID=%q", d.RuleID)
	}
}
