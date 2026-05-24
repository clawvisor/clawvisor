package taskrisk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestParseRiskResponse_Valid(t *testing.T) {
	raw := `{"risk_level":"high","explanation":"Auto-execute on send_email is risky.","factors":["auto_execute on high-sensitivity write"],"conflicts":[]}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.RiskLevel != "high" {
		t.Errorf("risk_level = %q, want %q", a.RiskLevel, "high")
	}
	if a.Explanation == "" {
		t.Error("explanation should not be empty")
	}
	if len(a.Factors) != 1 {
		t.Errorf("factors length = %d, want 1", len(a.Factors))
	}
}

func TestParseRiskResponse_MarkdownWrapped(t *testing.T) {
	raw := "```json\n{\"risk_level\":\"low\",\"explanation\":\"Read-only.\",\"factors\":[],\"conflicts\":[]}\n```"
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.RiskLevel != "low" {
		t.Errorf("risk_level = %q, want %q", a.RiskLevel, "low")
	}
}

func TestParseRiskResponse_InvalidJSON(t *testing.T) {
	_, err := parseRiskResponse("not json at all")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseRiskResponse_BadRiskLevel(t *testing.T) {
	raw := `{"risk_level":"extreme","explanation":"x","factors":[],"conflicts":[]}`
	_, err := parseRiskResponse(raw)
	if err == nil {
		t.Error("expected error for invalid risk_level")
	}
}

func TestParseRiskResponse_BadConflictSeverity(t *testing.T) {
	raw := `{"risk_level":"high","explanation":"x","factors":[],"conflicts":[{"field":"purpose","description":"mismatch","severity":"fatal"}]}`
	_, err := parseRiskResponse(raw)
	if err == nil {
		t.Error("expected error for invalid conflict severity")
	}
}

func TestParseRiskResponse_WithConflicts(t *testing.T) {
	raw := `{"risk_level":"critical","explanation":"Purpose mismatch.","factors":["wildcard scope"],"conflicts":[{"field":"expected_use","description":"contradicts purpose","severity":"error"}]}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.RiskLevel != "critical" {
		t.Errorf("risk_level = %q, want %q", a.RiskLevel, "critical")
	}
	if len(a.Conflicts) != 1 {
		t.Fatalf("conflicts length = %d, want 1", len(a.Conflicts))
	}
	if a.Conflicts[0].Severity != "error" {
		t.Errorf("conflict severity = %q, want %q", a.Conflicts[0].Severity, "error")
	}
}

func TestParseRiskResponse_IntentMatchYes(t *testing.T) {
	raw := `{"risk_level":"low","explanation":"Read-only.","factors":[],"conflicts":[],"intent_match":"yes","intent_match_explanation":"User asked to list calendar events."}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.IntentMatch != "yes" {
		t.Errorf("intent_match = %q, want %q", a.IntentMatch, "yes")
	}
	if a.IntentMatchExplanation == "" {
		t.Error("intent_match_explanation should not be empty")
	}
}

func TestParseRiskResponse_IntentMatchPartial(t *testing.T) {
	raw := `{"risk_level":"medium","explanation":"x","factors":[],"conflicts":[],"intent_match":"partial","intent_match_explanation":"User asked to read but task also writes."}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.IntentMatch != "partial" {
		t.Errorf("intent_match = %q, want %q", a.IntentMatch, "partial")
	}
}

func TestParseRiskResponse_IntentMatchAbsent(t *testing.T) {
	// Legacy / v1 responses omit intent_match — parser should collapse
	// to "unknown" rather than fail.
	raw := `{"risk_level":"low","explanation":"x","factors":[],"conflicts":[]}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.IntentMatch != "unknown" {
		t.Errorf("intent_match = %q, want %q (absent should collapse)", a.IntentMatch, "unknown")
	}
}

func TestParseRiskResponse_IntentMatchInvalid(t *testing.T) {
	// Unknown enum value should not fail the whole parse — the rest of
	// the risk read is still useful.
	raw := `{"risk_level":"low","explanation":"x","factors":[],"conflicts":[],"intent_match":"absolutely"}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.IntentMatch != "unknown" {
		t.Errorf("intent_match = %q, want %q (invalid should collapse)", a.IntentMatch, "unknown")
	}
}

func TestParseRiskResponse_IntentMatchCasing(t *testing.T) {
	// Models sometimes capitalize enums; normalize.
	raw := `{"risk_level":"low","explanation":"x","factors":[],"conflicts":[],"intent_match":"YES"}`
	a, err := parseRiskResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.IntentMatch != "yes" {
		t.Errorf("intent_match = %q, want %q", a.IntentMatch, "yes")
	}
}

func TestBuildAssessUserMessage_RecentUserTurns(t *testing.T) {
	msg := buildAssessUserMessage(AssessRequest{
		AgentName: "test-agent",
		Purpose:   "Check my calendar",
		AuthorizedActions: []store.TaskAction{
			{Service: "google.calendar", Action: "list_events", ExpectedUse: "list today's events"},
		},
		RecentUserTurns: []string{"can you check my calendar for tomorrow's events"},
	}, true)
	if !contains(msg, "Recent user turns") {
		t.Error("message should label the conversation block")
	}
	if !contains(msg, "tomorrow's events") {
		t.Error("message should include the actual user turn")
	}
	if !contains(msg, "UNTRUSTED") {
		t.Error("message should mark turns as UNTRUSTED for the assessor")
	}
}

func TestBuildAssessUserMessage_RecentUserTurns_FiltersBlank(t *testing.T) {
	// Whitespace-only turns must not produce empty quoted entries that
	// would mislead the assessor into "the user said nothing meaningful
	// twice."
	msg := buildAssessUserMessage(AssessRequest{
		AgentName:       "test-agent",
		Purpose:         "x",
		RecentUserTurns: []string{"", "  ", "real ask"},
	}, true)
	if !contains(msg, `(1, most recent last)`) {
		t.Errorf("blank turns should be filtered; message: %q", msg)
	}
}

func TestBuildAssessUserMessage_NoRecentUserTurns(t *testing.T) {
	// When the conversation block is absent, the assessor should not
	// see a "Recent user turns" header at all — that's the signal it
	// uses to emit intent_match=unknown.
	msg := buildAssessUserMessage(AssessRequest{
		AgentName: "test-agent",
		Purpose:   "x",
	}, true)
	if contains(msg, "Recent user turns") {
		t.Error("message should omit the recent-turns block when none provided")
	}
}

func TestBuildAssessUserMessage_Basic(t *testing.T) {
	msg := buildAssessUserMessage(AssessRequest{
		AgentName: "test-agent",
		Purpose:   "Check my calendar",
		AuthorizedActions: []store.TaskAction{
			{Service: "google.calendar", Action: "list_events", AutoExecute: true, ExpectedUse: "fetch today's events"},
		},
	}, true)
	if msg == "" {
		t.Fatal("message should not be empty")
	}
	for _, want := range []string{"test-agent", "Check my calendar", "google.calendar:list_events", "auto_execute=true", "fetch today's events"} {
		if !contains(msg, want) {
			t.Errorf("message missing %q", want)
		}
	}
}

func TestBuildAssessUserMessage_Wildcard(t *testing.T) {
	msg := buildAssessUserMessage(AssessRequest{
		AgentName: "bot",
		Purpose:   "Manage emails",
		AuthorizedActions: []store.TaskAction{
			{Service: "google.gmail", Action: "*", AutoExecute: true},
		},
	}, true)
	if !contains(msg, "google.gmail:*") {
		t.Error("message should contain wildcard action")
	}
}

func TestBuildAssessUserMessage_VerificationDisabledNote(t *testing.T) {
	req := AssessRequest{
		AgentName: "bot",
		Purpose:   "Manage emails",
		AuthorizedActions: []store.TaskAction{
			{Service: "google.gmail", Action: "send_message", AutoExecute: true},
		},
	}
	enabled := buildAssessUserMessage(req, true)
	if contains(enabled, "DEPLOYMENT NOTE") {
		t.Error("verification-enabled message should NOT contain deployment note")
	}
	disabled := buildAssessUserMessage(req, false)
	if !contains(disabled, "DEPLOYMENT NOTE: Intent verification is DISABLED") {
		t.Error("verification-disabled message should contain deployment note")
	}
}

func TestMarshalAssessment_Valid(t *testing.T) {
	a := &RiskAssessment{
		RiskLevel:   "low",
		Explanation: "Safe.",
		Factors:     []string{},
		Conflicts:   []ConflictDetail{},
	}
	raw := MarshalAssessment(a)
	if raw == nil {
		t.Fatal("expected non-nil result")
	}
	// Verify it's valid JSON.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["risk_level"] != "low" {
		t.Errorf("risk_level = %v, want %q", m["risk_level"], "low")
	}
}

func TestMarshalAssessment_Nil(t *testing.T) {
	raw := MarshalAssessment(nil)
	if raw != nil {
		t.Errorf("expected nil, got %s", raw)
	}
}

func TestBuildActionContextFromRegistry(t *testing.T) {
	// With a nil registry, buildActionContextFromRegistry should return empty.
	result := buildActionContextFromRegistry(context.Background(), nil, "")
	if result != "" {
		t.Errorf("expected empty string for nil registry, got %q", result)
	}
}

func TestNoopAssessor(t *testing.T) {
	a := NoopAssessor{}
	result, err := a.Assess(nil, AssessRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
