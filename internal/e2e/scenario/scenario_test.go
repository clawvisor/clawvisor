package scenario

import (
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestLoadRequiresIDAndGoal(t *testing.T) {
	dir := t.TempDir()

	missingID := filepath.Join(dir, "no-id.yaml")
	if err := writeFile(missingID, "goal: do a thing\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(missingID); err == nil {
		t.Fatal("expected error for missing id")
	}

	missingGoal := filepath.Join(dir, "no-goal.yaml")
	if err := writeFile(missingGoal, "id: x\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(missingGoal); err == nil {
		t.Fatal("expected error for missing goal")
	}

	complete := filepath.Join(dir, "complete.yaml")
	src := `
id: smoke
goal: do a thing
agent_name: ""
fixture:
  upstreams:
    - host: api.example.test
      fixtures: ./fixtures/example.json
  rules:
    - name: allow-status
      kind: egress
      action: allow
      host: api.example.test
      method: GET
      path: /status
approvals:
  policy: scripted
  default: deny
  rules:
    - match:
        host: api.example.test
        method: POST
      resolution: allow_once
budget:
  max_turns: 4
expectations:
  hard:
    - count:
        series: events.allow
        gte: 1
  soft:
    - "agent should explain what it did"
`
	if err := writeFile(complete, src); err != nil {
		t.Fatal(err)
	}
	s, err := Load(complete)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.ID != "smoke" || s.Goal != "do a thing" {
		t.Fatalf("unexpected scenario: %+v", s)
	}
	if s.AgentName != "e2e-smoke" {
		t.Fatalf("expected default agent name, got %q", s.AgentName)
	}
	if len(s.Fixture.Upstreams) != 1 || s.Fixture.Upstreams[0].Host != "api.example.test" {
		t.Fatalf("upstreams not parsed: %+v", s.Fixture.Upstreams)
	}
	if s.Approvals.Default != "deny" {
		t.Fatalf("approvals.default not parsed: %q", s.Approvals.Default)
	}
	if len(s.Expectations.Hard) != 1 || s.Expectations.Hard[0].Count == nil {
		t.Fatalf("hard expectation not parsed: %+v", s.Expectations.Hard)
	}
}

func TestEvaluateSeriesAndEventHas(t *testing.T) {
	allow := "allow"
	deny := "deny"
	approved := "approved"
	pendingResolution := ""
	resolvedAlways := "allow_always"
	now := nowPtr()
	events := []*store.RuntimeEvent{
		{EventType: "runtime.policy.allow_matched", Decision: &allow, Outcome: &approved},
		{EventType: "runtime.policy.deny_matched", Decision: &deny},
	}
	approvals := []*store.ApprovalRecord{
		{ID: "a", Resolution: resolvedAlways, ResolvedAt: now},
	}
	pending := []*store.ApprovalRecord{
		{ID: "b", Status: "pending", Resolution: pendingResolution},
	}
	snap := &Snapshot{
		Events:       events,
		Approvals:    approvals,
		Pending:      pending,
		ToolCalls:    3,
		FinalReply:   "All set: posted the report.",
		UpstreamHits: map[string]int{"api.example.test": 2},
	}
	one := 1
	two := 2
	three := 3
	exp := Expectations{
		Hard: []HardExpect{
			{Count: &CountExpect{Series: "events.total", GTE: &two}},
			{Count: &CountExpect{Series: "events.allow", EQ: &one}},
			{Count: &CountExpect{Series: "approvals.allow_always", EQ: &one}},
			{Count: &CountExpect{Series: "approvals.pending", LTE: &one}},
			{Count: &CountExpect{Series: "tool_calls", EQ: &three}},
			{Count: &CountExpect{Series: "upstream.api.example.test.hits", EQ: &two}},
			{EventHas: &EventExpect{EventType: "runtime.policy.deny_matched", Decision: "deny"}},
			{FinalAssistantContains: "posted"},
		},
	}
	if fails := Evaluate(exp, snap); len(fails) != 0 {
		t.Fatalf("expected no failures, got %+v", fails)
	}

	bad := Expectations{Hard: []HardExpect{
		{Count: &CountExpect{Series: "events.deny", EQ: &two}},                   // there is 1
		{EventHas: &EventExpect{EventType: "runtime.observe.would_block"}},       // missing
		{FinalAssistantContains: "rolled back"},                                  // missing
		{Count: &CountExpect{Series: "upstream.unregistered.test.hits", EQ: &one}}, // missing host
	}}
	fails := Evaluate(bad, snap)
	if len(fails) != 4 {
		t.Fatalf("expected 4 failures, got %d (%+v)", len(fails), fails)
	}
}

func writeFile(path, content string) error {
	return writeFileImpl(path, content)
}
