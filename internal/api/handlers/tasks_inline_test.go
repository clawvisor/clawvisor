package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// newInlineTasksHandlerForTest spins up a TasksHandler with the bare
// minimum dependencies (store, default config, logger) needed to drive
// CreateInlineApprovedTask end-to-end without touching adapters, the
// notifier, or the LLM verifier.
func newInlineTasksHandlerForTest(t *testing.T) (*TasksHandler, store.Store, *store.User, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "inline-tasks.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "inline-tasks@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "inline-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	cfg := config.Config{}
	cfg.Task.DefaultExpirySeconds = 600
	h := &TasksHandler{
		st:     st,
		cfg:    cfg,
		logger: slog.Default(),
	}
	return h, st, user, agent
}

// recordingAssessor is a stub that records each Assess call so the
// dedup test can verify the precomputed-assessment fast path skips
// the LLM round-trip.
type recordingAssessor struct {
	calls   int
	respond func() *taskrisk.RiskAssessment
}

func (r *recordingAssessor) Assess(_ context.Context, _ taskrisk.AssessRequest) (*taskrisk.RiskAssessment, error) {
	r.calls++
	if r.respond == nil {
		return &taskrisk.RiskAssessment{RiskLevel: "low"}, nil
	}
	return r.respond(), nil
}

func TestCreateInlineApprovedTaskWithAssessment_UsesPrecomputed(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	rec := &recordingAssessor{
		respond: func() *taskrisk.RiskAssessment { return &taskrisk.RiskAssessment{RiskLevel: "medium"} },
	}
	h.assessor = rec

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Make files",
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
	}
	// Precomputed gate verdict says "low" with a distinctive explanation.
	precomputed := &taskrisk.RiskAssessment{
		RiskLevel:   "low",
		Explanation: "precomputed-by-gate",
	}
	out, err := h.CreateInlineApprovedTaskWithAssessment(context.Background(), agent, req, "tu-1", precomputed)
	if err != nil {
		t.Fatalf("CreateInlineApprovedTaskWithAssessment: %v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil result")
	}
	if rec.calls != 0 {
		t.Errorf("assessor should NOT be called when precomputed is supplied (would defeat the dedup); got calls=%d", rec.calls)
	}
}

func TestCreateInlineApprovedTaskWithAssessment_NilPrecomputedFallsThrough(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	rec := &recordingAssessor{}
	h.assessor = rec

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Make files",
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
	}
	_, err := h.CreateInlineApprovedTaskWithAssessment(context.Background(), agent, req, "tu-2", nil)
	if err != nil {
		t.Fatalf("CreateInlineApprovedTaskWithAssessment(nil): %v", err)
	}
	if rec.calls != 1 {
		t.Errorf("assessor should be called once when precomputed is nil; got calls=%d", rec.calls)
	}
}

func TestCreateInlineApprovedTaskHappyPath(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()
	req := &runtimetasks.TaskCreateRequest{
		Purpose: "Build a landing page",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Create directories"},
			{ToolName: "Write", Why: "Create HTML"},
		},
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
	}

	out, err := h.CreateInlineApprovedTask(ctx, agent, req, "cv-origtoolxxxxxxxxxxxxxxxxxx")
	if err != nil {
		t.Fatalf("CreateInlineApprovedTask: %v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil result")
	}
	if out.Status != "active" {
		t.Errorf("status=%q, want active", out.Status)
	}
	if out.ApprovalSource != "inline_chat" {
		t.Errorf("approval_source=%q, want inline_chat", out.ApprovalSource)
	}
	if out.Lifetime != "session" {
		t.Errorf("lifetime=%q, want session (default)", out.Lifetime)
	}
	if out.ApprovalRecordID == "" {
		t.Error("expected non-empty approval_record_id")
	}
	if out.ExpiresAtRFC3339 == "" {
		t.Error("expected expires_at on a session task")
	}

	// Task row persisted with active status + inline_chat source.
	task, err := st.GetTask(ctx, out.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "active" || task.ApprovalSource != "inline_chat" {
		t.Errorf("task = status=%q source=%q; want active/inline_chat", task.Status, task.ApprovalSource)
	}
	if task.ApprovedAt == nil {
		t.Error("expected approved_at to be set")
	}
	if task.IntentVerificationMode != "strict" {
		t.Errorf("intent_verification_mode=%q, want strict", task.IntentVerificationMode)
	}
	if len(task.ExpectedTools) == 0 {
		t.Error("expected expected_tools to be persisted")
	}

	// Approval record persisted with inline_chat surface, resolved at creation.
	rec, err := st.GetApprovalRecord(ctx, out.ApprovalRecordID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if rec.Surface != "inline_chat" {
		t.Errorf("rec.Surface=%q, want inline_chat", rec.Surface)
	}
	if rec.Status != "approved" {
		t.Errorf("rec.Status=%q, want approved", rec.Status)
	}
	if rec.Resolution != "allow_session" {
		t.Errorf("rec.Resolution=%q, want allow_session", rec.Resolution)
	}
	if rec.ResolvedAt == nil {
		t.Error("rec.ResolvedAt should be set on inline approval")
	}
	if rec.Kind != "task_create" {
		t.Errorf("rec.Kind=%q, want task_create", rec.Kind)
	}
}

func TestCreateInlineApprovedTaskStandingLifetime(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()
	req := &runtimetasks.TaskCreateRequest{
		Purpose: "Long-running data ingest",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Ingest source files"},
		},
		Lifetime: "standing",
	}
	out, err := h.CreateInlineApprovedTask(ctx, agent, req, "")
	if err != nil {
		t.Fatalf("CreateInlineApprovedTask: %v", err)
	}
	if out.Lifetime != "standing" {
		t.Errorf("lifetime=%q, want standing", out.Lifetime)
	}
	if out.ExpiresAtRFC3339 != "" {
		t.Errorf("standing task should have no expires_at; got %q", out.ExpiresAtRFC3339)
	}
	rec, err := st.GetApprovalRecord(ctx, out.ApprovalRecordID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if rec.Resolution != "allow_always" {
		t.Errorf("rec.Resolution=%q, want allow_always for standing", rec.Resolution)
	}
}

func TestCreateInlineApprovedTaskReturnsCredentialPlaceholders(t *testing.T) {
	h, st, user, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	v := &stubVault{}
	if err := v.Set(ctx, user.ID, "agentphone", []byte("real-agentphone-token")); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	h.vault = v

	req := &runtimetasks.TaskCreateRequest{
		Purpose: "Place an outbound call with agentphone",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Call the agentphone API and verify the response"},
		},
		RequiredCredentials: []runtimetasks.RequiredCredential{
			{VaultItemID: "agentphone", Why: "Authenticate requests to the agentphone API"},
		},
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
	}

	out, err := h.CreateInlineApprovedTask(ctx, agent, req, "cv-origtoolxxxxxxxxxxxxxxxxxx")
	if err != nil {
		t.Fatalf("CreateInlineApprovedTask: %v", err)
	}
	if len(out.Credentials) != 1 {
		t.Fatalf("expected one credential placeholder, got %+v", out.Credentials)
	}
	cred := out.Credentials[0]
	if cred.VaultItemID != "agentphone" || cred.ServiceID != "agentphone" {
		t.Fatalf("unexpected credential metadata: %+v", cred)
	}
	if !strings.HasPrefix(cred.Placeholder, "autovault_agentphone_") {
		t.Fatalf("placeholder %q missing agentphone prefix", cred.Placeholder)
	}
	if cred.ExpiresAtRFC3339 == "" {
		t.Fatal("expected credential placeholder expiry")
	}
	if cred.CredentialGrantID == "" {
		t.Fatal("expected credential grant id")
	}

	meta, err := st.GetRuntimePlaceholder(ctx, cred.Placeholder)
	if err != nil {
		t.Fatalf("GetRuntimePlaceholder: %v", err)
	}
	if meta.TaskID != out.ID {
		t.Fatalf("placeholder TaskID=%q, want %q", meta.TaskID, out.ID)
	}
	if meta.CredentialGrantID != cred.CredentialGrantID {
		t.Fatalf("placeholder CredentialGrantID=%q, want %q", meta.CredentialGrantID, cred.CredentialGrantID)
	}
}

func TestActiveCredentialTaskApprovalRetryMintsMissingPlaceholders(t *testing.T) {
	h, st, user, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	v := &stubVault{}
	if err := v.Set(ctx, user.ID, "agentphone", []byte("real-agentphone-token")); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	h.vault = v

	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	task := &store.Task{
		ID:        "task-active-missing-credential",
		UserID:    user.ID,
		AgentID:   agent.ID,
		Purpose:   "Place an outbound call with agentphone",
		Status:    "active",
		Lifetime:  "session",
		ExpiresAt: &expiresAt,
		RequiredCredentials: json.RawMessage(
			`[{"vault_item_id":"agentphone","why":"Authenticate requests to the agentphone API"}]`,
		),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	w := httptest.NewRecorder()
	if !h.respondActiveCredentialApprovalRetry(ctx, w, user.ID, task) {
		t.Fatal("expected active credential task retry to be handled")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	placeholders, err := st.ListRuntimePlaceholders(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListRuntimePlaceholders: %v", err)
	}
	if len(placeholders) != 1 {
		t.Fatalf("expected one recovered placeholder, got %+v", placeholders)
	}
	if placeholders[0].TaskID != task.ID || placeholders[0].VaultItemID != "agentphone" {
		t.Fatalf("unexpected placeholder metadata: %+v", placeholders[0])
	}

	first := placeholders[0].Placeholder
	w = httptest.NewRecorder()
	if !h.respondActiveCredentialApprovalRetry(ctx, w, user.ID, task) {
		t.Fatal("expected second retry to be handled")
	}
	placeholders, err = st.ListRuntimePlaceholders(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListRuntimePlaceholders: %v", err)
	}
	if len(placeholders) != 1 || placeholders[0].Placeholder != first {
		t.Fatalf("retry should reuse existing placeholder, got %+v", placeholders)
	}
}

func TestActiveCredentialTaskApprovalRetryReplacesExpiredPlaceholder(t *testing.T) {
	h, st, user, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	v := &stubVault{}
	if err := v.Set(ctx, user.ID, "agentphone", []byte("real-agentphone-token")); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	h.vault = v

	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	task := &store.Task{
		ID:        "task-active-expired-credential",
		UserID:    user.ID,
		AgentID:   agent.ID,
		Purpose:   "Place an outbound call with agentphone",
		Status:    "active",
		Lifetime:  "session",
		ExpiresAt: &expiresAt,
		RequiredCredentials: json.RawMessage(
			`[{"vault_item_id":"agentphone","why":"Authenticate requests to the agentphone API"}]`,
		),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	expiredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	expiredGrant := &store.CredentialAuthorization{
		ID:            "expired-agentphone-grant",
		UserID:        user.ID,
		AgentID:       agent.ID,
		Scope:         "session",
		CredentialRef: "agentphone",
		Service:       "agentphone",
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		ExpiresAt:     &expiredAt,
	}
	if err := st.CreateCredentialAuthorization(ctx, expiredGrant); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	const expiredPlaceholder = "autovault_agentphone_expired"
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder:       expiredPlaceholder,
		UserID:            user.ID,
		AgentID:           agent.ID,
		ServiceID:         "agentphone",
		VaultItemID:       "agentphone",
		CredentialGrantID: expiredGrant.ID,
		TaskID:            task.ID,
		ExpiresAt:         &expiredAt,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}

	w := httptest.NewRecorder()
	if !h.respondActiveCredentialApprovalRetry(ctx, w, user.ID, task) {
		t.Fatal("expected active credential task retry to be handled")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	placeholders, err := st.ListRuntimePlaceholders(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListRuntimePlaceholders: %v", err)
	}
	valid := 0
	var validPlaceholder string
	now := time.Now().UTC()
	for _, placeholder := range placeholders {
		if placeholder.TaskID != task.ID || placeholder.VaultItemID != "agentphone" {
			continue
		}
		if _, ok := llmproxy.ValidateRuntimePlaceholderAccess(ctx, st, placeholder, user.ID, agent.ID, now); ok {
			valid++
			validPlaceholder = placeholder.Placeholder
		}
	}
	if valid != 1 {
		t.Fatalf("expected exactly one valid replacement placeholder, got valid=%d all=%+v", valid, placeholders)
	}
	if validPlaceholder == expiredPlaceholder {
		t.Fatalf("retry reused expired placeholder %q", validPlaceholder)
	}
}

func TestCreateInlineApprovedTaskRollsBackOnCredentialPlaceholderFailure(t *testing.T) {
	h, st, user, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	v := &stubVault{}
	if err := v.Set(ctx, user.ID, "agentphone", []byte("real-agentphone-token")); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}
	h.vault = v
	h.st = failingCredentialAuthorizationStore{Store: st, err: errors.New("forced credential authorization failure")}

	req := &runtimetasks.TaskCreateRequest{
		Purpose: "Place an outbound call with agentphone",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Call the agentphone API and verify the response"},
		},
		RequiredCredentials: []runtimetasks.RequiredCredential{
			{VaultItemID: "agentphone", Why: "Authenticate requests to the agentphone API"},
		},
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
	}

	_, err := h.CreateInlineApprovedTask(ctx, agent, req, "cv-origtoolxxxxxxxxxxxxxxxxxx")
	if err == nil || !strings.Contains(err.Error(), "mint credential placeholders") {
		t.Fatalf("expected credential mint failure, got %v", err)
	}
	tasks, _, err := st.ListTasks(ctx, user.ID, store.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one rolled-back task, got %+v", tasks)
	}
	if tasks[0].Status != "denied" {
		t.Fatalf("task should be denied after credential mint failure, got %+v", tasks[0])
	}
}

func TestCreateInlineApprovedTaskRejectsEmptyScope(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	req := &runtimetasks.TaskCreateRequest{
		Purpose: "Empty scope",
		// no expected_tools or expected_egress
	}
	_, err := h.CreateInlineApprovedTask(context.Background(), agent, req, "")
	if err == nil {
		t.Fatal("expected error on empty scope")
	}
}

func TestCreateInlineApprovedTaskRejectsEmptyPurpose(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	req := &runtimetasks.TaskCreateRequest{
		Purpose: "   ",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "x"},
		},
	}
	_, err := h.CreateInlineApprovedTask(context.Background(), agent, req, "")
	if err == nil {
		t.Fatal("expected error on empty purpose")
	}
}

func TestCreateInlineApprovedTaskRejectsStandingWithExpiry(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	req := &runtimetasks.TaskCreateRequest{
		Purpose: "x",
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "x"},
		},
		Lifetime:         "standing",
		ExpiresInSeconds: 600,
	}
	_, err := h.CreateInlineApprovedTask(context.Background(), agent, req, "")
	if err == nil {
		t.Fatal("expected error on standing+expiry")
	}
}
