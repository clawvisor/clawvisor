package llmproxy

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// fakeExpansionCreator stubs the InlineExpansionCreator + InlineTaskCreator
// interfaces with controllable outcomes so the intercept + resolver
// tests exercise the routing and side-effect plumbing without standing
// up a real handler.
type fakeExpansionCreator struct {
	mu sync.Mutex

	// CreatePendingErr, when non-nil, causes CreatePendingInlineExpansion
	// to fail. The intercept logs the audit reason and falls through.
	CreatePendingErr error
	// ApproveResult is what ApproveInlineExpansion returns when
	// ApproveErr is nil.
	ApproveResult *InlineApprovedExpansion
	ApproveErr    error
	// DenyErr is what DenyInlineExpansion returns.
	DenyErr error

	// Side-effect counters.
	CreatePendingCalls int
	ApproveCalls       int
	DenyCalls          int
	ExpireCalls        int

	// Inputs captured for assertion.
	LastPendingTaskID      string
	LastPendingReason      string
	LastPendingAddTools    int
	LastPendingPrecomputed *taskrisk.RiskAssessment
}

func (f *fakeExpansionCreator) CreatePendingInlineExpansion(
	_ context.Context,
	_ *store.Agent,
	taskID string,
	additions *runtimetasks.Envelope,
	reason string,
	precomputed *taskrisk.RiskAssessment,
) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatePendingCalls++
	f.LastPendingTaskID = taskID
	f.LastPendingReason = reason
	f.LastPendingPrecomputed = precomputed
	if additions != nil {
		f.LastPendingAddTools = len(additions.ExpectedTools)
	}
	if f.CreatePendingErr != nil {
		return "", f.CreatePendingErr
	}
	return taskID, nil
}

func (f *fakeExpansionCreator) ApproveInlineExpansion(_ context.Context, _, _ string) (*InlineApprovedExpansion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ApproveCalls++
	if f.ApproveErr != nil {
		return nil, f.ApproveErr
	}
	if f.ApproveResult != nil {
		return f.ApproveResult, nil
	}
	return &InlineApprovedExpansion{TaskID: "task-X", Status: "active"}, nil
}

func (f *fakeExpansionCreator) DenyInlineExpansion(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DenyCalls++
	return f.DenyErr
}

func (f *fakeExpansionCreator) ExpireInlineExpansion(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ExpireCalls++
	return nil
}

// CreateInlineApprovedTask is unused by the expansion intercept; the
// type assertion only consults InlineExpansionCreator. Implement it as
// a no-op so the value still satisfies the umbrella InlineTaskCreator
// interface the pipeline cfg field types to.
func (f *fakeExpansionCreator) CreateInlineApprovedTask(_ context.Context, _ *store.Agent, _ *runtimetasks.TaskCreateRequest, _ string) (*InlineApprovedTask, error) {
	return nil, errors.New("not implemented in fakeExpansionCreator")
}

// TestResolveInlineExpansionApproval_ApprovePath confirms a chat-side
// "approve" against a StageAwaitingExpansionApproval hold calls
// ApproveInlineExpansion and emits the approved-augmentation notice
// (the "scope was expanded" body, not the task-creation body).
func TestResolveInlineExpansionApproval_ApprovePath(t *testing.T) {
	fc := &fakeExpansionCreator{
		ApproveResult: &InlineApprovedExpansion{TaskID: "task-X", Status: "active", Purpose: "test"},
	}
	hold := &PendingLiteApproval{
		ID:              "cv-test",
		UserID:          "user-1",
		AgentID:         "agent-1",
		ExpansionTaskID: "task-X",
	}
	req := InlineApprovalRewriteRequest{
		Agent:   &store.Agent{ID: "agent-1", UserID: "user-1"},
		Creator: fc,
	}
	body, out := resolveInlineExpansionApproval(context.Background(), req, hold, "approve")
	if out.Decision != "allow" {
		t.Errorf("Decision = %q, want allow; out=%+v", out.Decision, out)
	}
	if out.TaskID != "task-X" {
		t.Errorf("TaskID = %q, want task-X", out.TaskID)
	}
	if fc.ApproveCalls != 1 {
		t.Errorf("ApproveInlineExpansion calls = %d, want 1", fc.ApproveCalls)
	}
	if !strings.Contains(body, "scope was expanded") {
		t.Errorf("body should describe scope expansion, got:\n%s", body)
	}
	// The task-creation augmentation says "task was created"; the
	// expansion path must NOT emit that text or the model will believe
	// a fresh task was minted.
	if strings.Contains(body, "Task was created and approved by the user") {
		t.Errorf("expansion body contained task-creation text; renderers crossed wires:\n%s", body)
	}
}

// TestResolveInlineExpansionApproval_DenyPath confirms the deny verb
// calls DenyInlineExpansion and emits the expansion-denied notice.
func TestResolveInlineExpansionApproval_DenyPath(t *testing.T) {
	fc := &fakeExpansionCreator{}
	hold := &PendingLiteApproval{
		ID:              "cv-test",
		UserID:          "user-1",
		AgentID:         "agent-1",
		ExpansionTaskID: "task-X",
	}
	req := InlineApprovalRewriteRequest{
		Agent:   &store.Agent{ID: "agent-1", UserID: "user-1"},
		Creator: fc,
	}
	body, out := resolveInlineExpansionApproval(context.Background(), req, hold, "deny")
	if out.Decision != "deny" {
		t.Errorf("Decision = %q, want deny", out.Decision)
	}
	if fc.DenyCalls != 1 {
		t.Errorf("DenyInlineExpansion calls = %d, want 1", fc.DenyCalls)
	}
	if !strings.Contains(body, "denied the scope-expansion request") {
		t.Errorf("body should describe expansion denial, got:\n%s", body)
	}
}

// TestResolveInlineExpansionApproval_AlreadyTerminal exercises the
// already-resolved race: dashboard or notifier resolved the expansion
// before the chat reply landed. The resolver must surface the typed
// error as the dedicated "already terminal" notice rather than the
// generic creator-error path.
func TestResolveInlineExpansionApproval_AlreadyTerminal(t *testing.T) {
	fc := &fakeExpansionCreator{
		ApproveErr: &ErrInlineExpansionAlreadyTerminal{Status: "denied"},
	}
	hold := &PendingLiteApproval{
		ID:              "cv-test",
		UserID:          "user-1",
		AgentID:         "agent-1",
		ExpansionTaskID: "task-X",
	}
	req := InlineApprovalRewriteRequest{
		Agent:   &store.Agent{ID: "agent-1", UserID: "user-1"},
		Creator: fc,
	}
	body, out := resolveInlineExpansionApproval(context.Background(), req, hold, "approve")
	if out.Decision != "deny" {
		t.Errorf("Decision = %q, want deny on terminal race", out.Decision)
	}
	if out.Outcome != "inline_expansion_already_terminal" {
		t.Errorf("Outcome = %q, want inline_expansion_already_terminal", out.Outcome)
	}
	if !strings.Contains(body, "already") {
		t.Errorf("body should describe the already-resolved race, got:\n%s", body)
	}
}

// TestRenderExpansionApprovalPrompt_RisksRender confirms the
// expansion prompt surfaces the merged-envelope risk level (and
// explanation when present) in the same shape the task-creation
// prompt uses. Without this the reviewer would only see the
// addition + lifetime — broadening to a "high"-risk scope would
// land silently.
func TestRenderExpansionApprovalPrompt_RisksRender(t *testing.T) {
	additions := &runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "bash", Why: "Run git commit and push."},
		},
	}
	risk := &taskrisk.RiskAssessment{
		RiskLevel:   "high",
		Explanation: "Adds shell access to a previously read-only task.",
	}
	prompt := renderExpansionApprovalPrompt(additions, "land the change", "Refactor src/foo.go", "task-abc", "session", risk, "cv-aaa")
	if !strings.Contains(prompt, "Risk") {
		t.Errorf("prompt missing risk section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "high") {
		t.Errorf("prompt missing risk level:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Adds shell access") {
		t.Errorf("prompt missing risk explanation:\n%s", prompt)
	}
}

// TestRenderExpansionApprovalPrompt_NoRiskSilent guards the
// inverse: when the assessor returns nothing usable, the prompt
// must NOT render an empty "Risk" header. Avoids the
// "Risk:" + blank case the reviewer would otherwise see when the
// assessor is unconfigured or returns "unknown" / empty.
func TestRenderExpansionApprovalPrompt_NoRiskSilent(t *testing.T) {
	additions := &runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{ToolName: "edit", Why: "small fix"}},
	}
	prompt := renderExpansionApprovalPrompt(additions, "land it", "purpose", "task-abc", "session", nil, "cv-aaa")
	if strings.Contains(prompt, "\nRisk\n") || strings.Contains(prompt, "\n\nRisk\n") {
		t.Errorf("prompt rendered empty Risk section:\n%s", prompt)
	}
}

// TestAssessInlineExpansionRisk_LLMVerdictReturnedAsIs pins the
// happy path: when the assessor is configured and returns a usable
// verdict, that verdict is returned with its RiskLevel + Explanation
// intact. The helper does NOT merge with the deterministic floor —
// inline expansion deliberately mirrors assessInlineTaskRisk so both
// inline surfaces (creation + expansion) trust the LLM read directly.
func TestAssessInlineExpansionRisk_LLMVerdictReturnedAsIs(t *testing.T) {
	cfg := PostprocessConfig{
		ApprovalContext: ApprovalContext{
			TaskRiskAssessor: &mockTaskRiskAssessor{verdict: &TaskRiskAssessment{
				RiskLevel:   "high",
				Explanation: "Mutating egress to a previously read-only host.",
			}},
		},
	}
	merged := runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{ToolName: "edit", Why: "Update README.md to fix typo"}},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/x", nil)
	got := assessInlineExpansionRisk(httpReq, cfg, "doc tweak", merged, func(string, ...any) {})
	if got == nil {
		t.Fatal("expected non-nil assessment")
	}
	if got.RiskLevel != "high" {
		t.Errorf("RiskLevel = %q, want the LLM's high verdict returned as-is", got.RiskLevel)
	}
	if !strings.Contains(got.Explanation, "Mutating egress") {
		t.Errorf("Explanation lost the LLM text: %q", got.Explanation)
	}
}

// TestAssessInlineExpansionRisk_FloorDoesNotRaiseLLMVerdict pins the
// no-merge contract that aligns inline expansion with inline
// creation: when the LLM returns a usable low verdict on an envelope
// whose deterministic floor would have flagged high, the LLM verdict
// wins outright — the floor is NOT a backstop on this path.
//
// IntentVerificationMode="off" drives the deterministic floor to
// high (see internal/runtime/policy/envelope_risk.go). Before the
// alignment with assessInlineTaskRisk, the merge rule would have
// raised the LLM's "low" to "high"; now the LLM verdict is
// authoritative.
func TestAssessInlineExpansionRisk_FloorDoesNotRaiseLLMVerdict(t *testing.T) {
	cfg := PostprocessConfig{
		ApprovalContext: ApprovalContext{
			TaskRiskAssessor: &mockTaskRiskAssessor{verdict: &TaskRiskAssessment{
				RiskLevel:   "low",
				Explanation: "LLM judged this addition trivial.",
			}},
		},
	}
	merged := runtimetasks.Envelope{
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "edit", Why: "Update README.md to fix typo"}},
		IntentVerificationMode: "off", // drives the deterministic floor to high
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/x", nil)
	got := assessInlineExpansionRisk(httpReq, cfg, "doc tweak", merged, func(string, ...any) {})
	if got == nil {
		t.Fatal("expected non-nil assessment")
	}
	if got.RiskLevel != "low" {
		t.Errorf("RiskLevel = %q, want low (LLM verdict wins; floor must not raise)", got.RiskLevel)
	}
	if !strings.Contains(got.Explanation, "LLM judged") {
		t.Errorf("Explanation lost the LLM text: %q (floor must not displace LLM)", got.Explanation)
	}
}

// TestAssessInlineExpansionRisk_UnknownFallsBackToFloor pins the
// "LLM unavailable / spend cap exhausted" path: the assessor returns
// the sentinel "unknown" level and we must fall back to the
// deterministic floor rather than persisting "unknown" — that would
// strip the risk badge from the inline prompt entirely.
func TestAssessInlineExpansionRisk_UnknownFallsBackToFloor(t *testing.T) {
	cfg := PostprocessConfig{
		ApprovalContext: ApprovalContext{
			TaskRiskAssessor: &mockTaskRiskAssessor{verdict: &TaskRiskAssessment{RiskLevel: "unknown"}},
		},
	}
	merged := runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{ToolName: "bash", Why: ""}},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/x", nil)
	got := assessInlineExpansionRisk(httpReq, cfg, "p", merged, func(string, ...any) {})
	if got == nil {
		t.Fatal("expected the deterministic floor when LLM returns unknown")
	}
	if strings.EqualFold(got.RiskLevel, "unknown") || got.RiskLevel == "" {
		t.Errorf("RiskLevel = %q, want a real level from the floor", got.RiskLevel)
	}
}

// TestAssessInlineExpansionRisk_AssessorNilFallsBackToFloor pins the
// boot-time path where TaskRiskAssessor was never wired — common on
// daemons where the LLM creds aren't configured yet. The floor must
// still score the envelope so the prompt has a level.
func TestAssessInlineExpansionRisk_AssessorNilFallsBackToFloor(t *testing.T) {
	cfg := PostprocessConfig{}
	merged := runtimetasks.Envelope{
		ExpectedTools: []runtimetasks.ExpectedTool{{ToolName: "bash", Why: ""}},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/x", nil)
	got := assessInlineExpansionRisk(httpReq, cfg, "p", merged, func(string, ...any) {})
	if got == nil {
		t.Fatal("expected the deterministic floor when assessor is unconfigured")
	}
	if got.RiskLevel == "" {
		t.Errorf("RiskLevel must be non-empty when the floor produced an assessment")
	}
}

// TestMaybeInterceptInlineExpansion_QuerySignalRequired confirms the
// intercept is dormant without ?surface=inline — exactly the same
// opt-in shape as the task-creation intercept, so a headless agent
// keeps routing through the dashboard handler unchanged.
func TestMaybeInterceptInlineExpansion_QuerySignalRequired(t *testing.T) {
	fc := &fakeExpansionCreator{}
	cfg := PostprocessConfig{
		ApprovalContext: ApprovalContext{
			PendingApprovals:  NewMemoryPendingApprovalCache(0),
			InlineTaskCreator: fc,
		},
		AgentContext: AgentContext{
			AgentID:     "agent-1",
			AgentUserID: "user-1",
		},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks/task-X/expand", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}
	tu := conversation.ToolUse{ID: "tu-1"}

	_, claimed := MaybeInterceptInlineExpansion(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
	if claimed {
		t.Fatalf("intercept claimed without ?surface=inline; opt-in signal is mandatory")
	}
	if fc.CreatePendingCalls != 0 {
		t.Fatalf("creator was called without opt-in signal; intercept should bail early")
	}
}
