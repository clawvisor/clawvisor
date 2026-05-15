package handlers

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
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
