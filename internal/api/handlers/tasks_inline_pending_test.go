package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestCreatePendingInlineTask_LandsRowAndCanonicalRecord verifies the
// intercept-side helper: the task is persisted at pending_approval with
// approval_source=inline_chat, and the canonical approval_records row
// is created at status=pending with surface=inline_chat. No credential
// placeholders should be minted yet (those land at the approve
// transition).
func TestCreatePendingInlineTask_LandsRowAndCanonicalRecord(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Build a landing page",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Create dir"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}
	if taskID == "" {
		t.Fatal("CreatePendingInlineTask returned empty taskID")
	}

	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "pending_approval" {
		t.Errorf("status=%q, want pending_approval", got.Status)
	}
	if got.ApprovalSource != "inline_chat" {
		t.Errorf("approval_source=%q, want inline_chat", got.ApprovalSource)
	}
	if got.ApprovedAt != nil {
		t.Errorf("approved_at should be nil at pending creation; got %v", got.ApprovedAt)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at should be nil at pending creation (scope window starts at approve); got %v", got.ExpiresAt)
	}

	// Canonical record landed as pending with surface=inline_chat.
	recs, err := st.ListPendingApprovalRecords(ctx, agent.UserID)
	if err != nil {
		t.Fatalf("ListPendingApprovalRecords: %v", err)
	}
	var rec *store.ApprovalRecord
	for _, r := range recs {
		if r.TaskID != nil && *r.TaskID == taskID {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatal("expected canonical pending approval record for the new task")
	}
	if rec.Status != "pending" {
		t.Errorf("canonical record status=%q, want pending", rec.Status)
	}
	if rec.Surface != "inline_chat" {
		t.Errorf("canonical record surface=%q, want inline_chat", rec.Surface)
	}
	if rec.Resolution != "" {
		t.Errorf("canonical record resolution=%q, want empty at pending time", rec.Resolution)
	}
}

// TestApproveInlineTask_TransitionsPendingToActive exercises the chat
// approve transition: the existing pending task flips to active,
// returns InlineApprovedTask, and the canonical record flips to
// approved. The dashboard guard (errInlineChatBound) is bypassed
// because the chat surface is the legitimate caller.
func TestApproveInlineTask_TransitionsPendingToActive(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Build a landing page",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Create dir"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}

	out, err := h.ApproveInlineTask(ctx, taskID, agent.UserID)
	if err != nil {
		t.Fatalf("ApproveInlineTask: %v", err)
	}
	if out == nil || out.ID != taskID {
		t.Fatalf("InlineApprovedTask.ID=%q, want %q", out.ID, taskID)
	}
	if out.Status != "active" {
		t.Errorf("InlineApprovedTask.Status=%q, want active", out.Status)
	}
	if out.ApprovalRecordID == "" {
		t.Errorf("InlineApprovedTask.ApprovalRecordID empty; the canonical record id should round-trip into the response so the LLM-side audit chain sees it")
	}

	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "active" {
		t.Errorf("task.Status=%q after approve, want active", got.Status)
	}
	if got.ApprovedAt == nil {
		t.Error("task.ApprovedAt should be set after approve")
	}
	if got.ExpiresAt == nil {
		t.Error("task.ExpiresAt should be set after approve (scope window starts now)")
	}
	if got.ApprovalSource != "inline_chat" {
		t.Errorf("task.ApprovalSource=%q, want inline_chat (preserved on transition)", got.ApprovalSource)
	}

	// Canonical record flipped from pending to approved.
	recs, _ := st.ListPendingApprovalRecords(ctx, agent.UserID)
	for _, r := range recs {
		if r.TaskID != nil && *r.TaskID == taskID {
			t.Errorf("pending approval record should be gone (resolved); got %+v", r)
		}
	}
}

// TestDenyInlineTask_FlipsPendingToDenied verifies the deny side of
// the chat resolution path.
func TestDenyInlineTask_FlipsPendingToDenied(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Make files",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}

	if err := h.DenyInlineTask(ctx, taskID, agent.UserID); err != nil {
		t.Fatalf("DenyInlineTask: %v", err)
	}

	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "denied" {
		t.Errorf("task.Status=%q after deny, want denied", got.Status)
	}
}

// TestApproveByTaskID_RefusesInlineChatPending verifies the dashboard
// surface guard: a chat-bound pending task returns errInlineChatBound
// when called via the standard ApproveByTaskID path (used by both the
// HTTP handler and the Telegram notifier callback). The chat surface
// must call ApproveInlineTask instead.
func TestApproveByTaskID_RefusesInlineChatPending(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Make files",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}

	if err := h.ApproveByTaskID(ctx, taskID, agent.UserID); !errors.Is(err, errInlineChatBound) {
		t.Fatalf("ApproveByTaskID: err=%v, want errInlineChatBound", err)
	}
	if err := h.DenyByTaskID(ctx, taskID, agent.UserID); !errors.Is(err, errInlineChatBound) {
		t.Fatalf("DenyByTaskID: err=%v, want errInlineChatBound", err)
	}
}

// TestApprove_HTTPRefusesInlineChatPending verifies the HTTP layer
// returns 409 INLINE_CHAT_BOUND for chat-bound pending tasks.
func TestApprove_HTTPRefusesInlineChatPending(t *testing.T) {
	h, _, user, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Make files",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}

	// Approve.
	r := httptest.NewRequest("POST", "/api/tasks/"+taskID+"/approve", nil)
	r.SetPathValue("id", taskID)
	r = r.WithContext(context.WithValue(r.Context(), middleware.UserContextKey, user))
	w := httptest.NewRecorder()
	h.Approve(w, r)
	if w.Code != 409 {
		t.Fatalf("Approve status=%d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INLINE_CHAT_BOUND") {
		t.Errorf("Approve body missing INLINE_CHAT_BOUND; got %s", w.Body.String())
	}

	// Deny.
	r = httptest.NewRequest("POST", "/api/tasks/"+taskID+"/deny", nil)
	r.SetPathValue("id", taskID)
	r = r.WithContext(context.WithValue(r.Context(), middleware.UserContextKey, user))
	w = httptest.NewRecorder()
	h.Deny(w, r)
	if w.Code != 409 {
		t.Fatalf("Deny status=%d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INLINE_CHAT_BOUND") {
		t.Errorf("Deny body missing INLINE_CHAT_BOUND; got %s", w.Body.String())
	}
	_ = ctx
}
