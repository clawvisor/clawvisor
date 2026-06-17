package policies_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestControlNotice_SkipsEmptyControlBaseURL pins the no-config gate.
func TestControlNotice_SkipsEmptyControlBaseURL(t *testing.T) {
	p := policies.NewControlNotice("", noopAvailableTools, noopToolRules)
	req := newTestRequestForControlNotice(`{"model":"claude-sonnet-4","tools":[{"name":"Bash"}],"messages":[]}`)
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
}

// TestControlNotice_SkipsCountTokensPath pins the path gate.
func TestControlNotice_SkipsCountTokensPath(t *testing.T) {
	p := policies.NewControlNotice("http://localhost:25297", oneToolAvailable, noopToolRules)
	req := newTestRequestForControlNotice(`{"model":"claude-sonnet-4","tools":[{"name":"Bash"}],"messages":[]}`)
	req.HTTPRequest().URL.Path = "/v1/messages/count_tokens"
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
}

// TestControlNotice_SkipsWhenNoTools pins the no-tools gate.
func TestControlNotice_SkipsWhenNoTools(t *testing.T) {
	p := policies.NewControlNotice("http://localhost:25297", emptyToolsAvailable, noopToolRules)
	req := newTestRequestForControlNotice(`{"model":"claude-sonnet-4","messages":[]}`)
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Errorf("Outcome = %q, want Skip", verdict.Outcome)
	}
}

// TestControlNotice_InjectsWhenGatesPass verifies the migration path:
// proper URL + tools[] declared → notice injected, audit flag set.
func TestControlNotice_InjectsWhenGatesPass(t *testing.T) {
	p := policies.NewControlNotice("http://localhost:25297", oneToolAvailable, noopToolRules)
	req := newTestRequestForControlNotice(`{"model":"claude-sonnet-4","tools":[{"name":"Bash"}],"messages":[]}`)
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeAllow {
		t.Errorf("Outcome = %q, want Allow", verdict.Outcome)
	}
	if len(mut.ReplaceBodyCalls) != 1 {
		t.Fatalf("expected 1 ReplaceBody, got %d", len(mut.ReplaceBodyCalls))
	}
	if v := verdict.AuditParams["control_notice_injected"]; v != true {
		t.Errorf("audit flag control_notice_injected = %v, want true", v)
	}
	// Quick smoke check that the notice text landed.
	if !strings.Contains(string(mut.ReplaceBodyCalls[0]), "Clawvisor proxy-lite control plane") {
		t.Errorf("notice text not in replaced body")
	}
}

// TestControlNotice_EarlyExitWhenNoticeAlreadyPresent pins the
// performance fix: on turns 2+ the client returns the system prompt
// with the sentinel already pinned, and Preprocess must short-circuit
// before invoking the tool-rules / active-tasks loaders. Without the
// early exit those loaders run on every turn and the result is
// thrown away by the dedup inside InjectControlNoticeWithSnapshot.
func TestControlNotice_EarlyExitWhenNoticeAlreadyPresent(t *testing.T) {
	toolRulesCalls := 0
	loadToolRules := func(_ context.Context, _, _ string) []*store.RuntimePolicyRule {
		toolRulesCalls++
		return nil
	}
	activeTasksCalls := 0
	loadActiveTasks := func(_ context.Context, _, _, _ string) string {
		activeTasksCalls++
		return "  - 00000000 · purpose=\"stale\" · lifetime=standing · expires=never"
	}
	p := policies.NewControlNoticeWithSnapshot("http://localhost:25297", oneToolAvailable, loadToolRules, loadActiveTasks)

	// System prompt already carries the sentinel — same shape as a
	// second-turn request after the first turn injected the notice.
	body := `{"model":"claude-sonnet-4","tools":[{"name":"Bash"}],"system":"Clawvisor proxy-lite control plane.\nrest of stale notice","messages":[]}`
	req := newTestRequestForControlNotice(body)
	mut := &recordingRequestMutator{}
	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeSkip {
		t.Fatalf("Outcome = %q, want Skip", verdict.Outcome)
	}
	if toolRulesCalls != 0 {
		t.Errorf("loadToolRules invoked %d times despite sentinel-present early-exit", toolRulesCalls)
	}
	if activeTasksCalls != 0 {
		t.Errorf("loadActiveTasks invoked %d times despite sentinel-present early-exit", activeTasksCalls)
	}
	if len(mut.ReplaceBodyCalls) != 0 {
		t.Errorf("ReplaceBody invoked %d times on dedup; want 0", len(mut.ReplaceBodyCalls))
	}
}

func TestControlNotice_DenyKeepsRawErrorInAuditOnly(t *testing.T) {
	p := policies.NewControlNotice("http://localhost:25297", oneToolAvailable, noopToolRules)
	req := newTestRequestForControlNotice(`{not valid json`)
	mut := &recordingRequestMutator{}

	verdict, err := p.Preprocess(context.Background(), req, mut)
	if err != nil {
		t.Fatalf("Preprocess: %v", err)
	}
	if verdict.Outcome != pipeline.OutcomeDeny {
		t.Fatalf("Outcome = %q, want Deny", verdict.Outcome)
	}
	if got, _ := verdict.AuditParams["control_notice_error"].(string); got == "" || !strings.Contains(got, "invalid character") {
		t.Fatalf("control_notice_error audit field = %q, want raw parse detail", got)
	}
	if strings.Contains(verdict.Reason, "invalid character") {
		t.Fatalf("model-facing reason leaked raw parse detail: %q", verdict.Reason)
	}
}

// --- test helpers ---

func newTestRequestForControlNotice(body string) *stubReadOnlyRequest {
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	return &stubReadOnlyRequest{
		provider:        conversation.ProviderAnthropic,
		rawBody:         []byte(body),
		httpReqOverride: httpReq,
	}
}

// noopAvailableTools / oneToolAvailable / emptyToolsAvailable are
// AvailableToolsFn test doubles.
func noopAvailableTools(_ conversation.Provider, _ []byte) []string  { return nil }
func emptyToolsAvailable(_ conversation.Provider, _ []byte) []string { return nil }
func oneToolAvailable(_ conversation.Provider, _ []byte) []string    { return []string{"Bash"} }

// noopToolRules returns nil rules — the underlying notice builder
// renders the no-policy variant.
func noopToolRules(_ context.Context, _, _ string) []*store.RuntimePolicyRule { return nil }
