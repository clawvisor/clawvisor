package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/postproc"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
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

func TestCreatePendingInlineTask_StandingLifetimeStoresZeroExpiresIn(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Keep ingest running",
		IntentVerificationMode: "strict",
		Lifetime:               "standing",
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run ingest"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}
	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ExpiresInSeconds != 0 {
		t.Errorf("standing pending task ExpiresInSeconds=%d, want 0", got.ExpiresInSeconds)
	}
}

func TestCreatePendingInlineTask_RecordFailureExpiresTask(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	h.st = &createApprovalRecordFailStore{Store: st}
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Build a landing page",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Create dir"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err == nil {
		t.Fatalf("CreatePendingInlineTask succeeded, want approval-record error")
	}
	if taskID != "" {
		t.Fatalf("CreatePendingInlineTask taskID=%q, want empty on error", taskID)
	}

	tasks, _, err := st.ListTasks(ctx, agent.UserID, store.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks len=%d, want 1", len(tasks))
	}
	if tasks[0].Status != "expired" {
		t.Fatalf("rollback status=%q, want expired for operational record failure", tasks[0].Status)
	}
}

type createApprovalRecordFailStore struct {
	store.Store
}

func (s *createApprovalRecordFailStore) CreateApprovalRecord(context.Context, *store.ApprovalRecord) error {
	return errors.New("forced approval record failure")
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
	recs, err := st.ListPendingApprovalRecords(ctx, agent.UserID)
	if err != nil {
		t.Fatalf("ListPendingApprovalRecords: %v", err)
	}
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

func TestPostprocess_ReplayFailureExpiresPendingInlineTask(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	taskBody := `{"purpose":"Rollback user's replay failure task","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Create files"}]}`
	req := httptest.NewRequest("POST", "/api/v1/messages?beta=true", nil)
	cache := &failingInlineTaskHoldCache{err: errors.New("simulated replay hold failure")}
	result := postproc.Postprocess(req, anthropicInlineTaskPostBody(t, taskBody), "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineToolUseEvaluatorFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: agent.UserID,
			AgentID: agent.ID,
			AgentName: agent.Name,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
			InlineTaskCreator: h,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector: inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
			RewriteOpts: inspector.DefaultRewriteOpts("http://localhost:25297"),
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if result.Body != nil {
		t.Fatalf("Postprocess should fail closed after replay failure; got %d body bytes", len(result.Body))
	}
	if !strings.Contains(result.SkippedReason, "approval hold storage failed") {
		t.Fatalf("SkippedReason=%q, want approval hold storage failure", result.SkippedReason)
	}
	if cache.holdCalls != 1 {
		t.Fatalf("underlying cache Hold calls=%d, want replay to hit failing cache once", cache.holdCalls)
	}

	tasks, _, err := st.ListTasks(ctx, agent.UserID, store.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var got *store.Task
	for _, task := range tasks {
		if task.Purpose == "Rollback user's replay failure task" {
			got = task
			break
		}
	}
	if got == nil {
		t.Fatal("expected pending inline task row to be created before replay failure")
	}
	if got.Status != "expired" {
		t.Fatalf("task status=%q, want expired rollback state", got.Status)
	}
	if got.ApprovalSource != "inline_chat" {
		t.Fatalf("approval_source=%q, want inline_chat", got.ApprovalSource)
	}
}

func TestPostprocess_RewriterFailureExpiresPendingInlineTask(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	taskBody := `{"purpose":"Rollback user's rewriter failure task","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Create files"}]}`
	req := httptest.NewRequest("POST", "/api/v1/messages?beta=true", nil)
	result := postproc.Postprocess(req, anthropicInlineTaskPostBody(t, taskBody), "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineToolUseEvaluatorFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: agent.UserID,
			AgentID: agent.ID,
			AgentName: agent.Name,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: llmproxy.NewMemoryPendingApprovalCache(time.Minute),
			InlineTaskCreator: h,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector: inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
			RewriteOpts: inspector.DefaultRewriteOpts("http://localhost:25297"),
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
			ResponseRegistry: conversation.NewResponseRegistry(evalThenErrorRewriter{}),
		},
	})

	if result.Body != nil {
		t.Fatalf("Postprocess should fail closed after rewriter failure; got %d body bytes", len(result.Body))
	}
	if !strings.Contains(result.SkippedReason, "rewriter error") {
		t.Fatalf("SkippedReason=%q, want rewriter error", result.SkippedReason)
	}

	tasks, _, err := st.ListTasks(ctx, agent.UserID, store.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var got *store.Task
	for _, task := range tasks {
		if task.Purpose == "Rollback user's rewriter failure task" {
			got = task
			break
		}
	}
	if got == nil {
		t.Fatal("expected pending inline task row to be created before rewriter failure")
	}
	if got.Status != "expired" {
		t.Fatalf("task status=%q, want expired rollback state", got.Status)
	}
	if got.ApprovalSource != "inline_chat" {
		t.Fatalf("approval_source=%q, want inline_chat", got.ApprovalSource)
	}
}

// TestApproveByTaskID_RefusesInlineChatPending verifies the asymmetric
// guard: ApproveByTaskID refuses chat-bound pending rows with
// errInlineChatBound (the chat surface must drive approval so the
// model sees the substituted reply), but DenyByTaskID intentionally
// permits dismissal so a user can clear a zombie task the agent has
// lost track of.
func TestApproveByTaskID_RefusesInlineChatPending(t *testing.T) {
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

	if err := h.ApproveByTaskID(ctx, taskID, agent.UserID); !errors.Is(err, errInlineChatBound) {
		t.Fatalf("ApproveByTaskID: err=%v, want errInlineChatBound", err)
	}
	// Deny is permitted on chat-bound rows so a zombie task can be
	// dismissed; verify it lands the row at "denied".
	if err := h.DenyByTaskID(ctx, taskID, agent.UserID); err != nil {
		t.Fatalf("DenyByTaskID: %v", err)
	}
	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "denied" {
		t.Errorf("status=%q after DenyByTaskID, want denied", got.Status)
	}
}

// TestApprove_HTTPRefusesInlineChatPending verifies the HTTP layer:
// Approve still returns 409 INLINE_CHAT_BOUND for chat-bound pending
// tasks, but Deny succeeds (matching the dashboard UX where the user
// can dismiss a zombie chat-bound task).
func TestApprove_HTTPRefusesInlineChatPending(t *testing.T) {
	h, st, user, agent := newInlineTasksHandlerForTest(t)
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

	// Approve still 409s.
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

	// Deny succeeds — the dashboard is allowed to dismiss zombie
	// chat-bound rows so users aren't stuck waiting on a task the
	// agent has lost track of.
	r = httptest.NewRequest("POST", "/api/tasks/"+taskID+"/deny", nil)
	r.SetPathValue("id", taskID)
	r = r.WithContext(context.WithValue(r.Context(), middleware.UserContextKey, user))
	w = httptest.NewRecorder()
	h.Deny(w, r)
	if w.Code != 200 {
		t.Fatalf("Deny status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after Deny: %v", err)
	}
	if got.Status != "denied" {
		t.Errorf("status=%q after HTTP Deny, want denied", got.Status)
	}
}

// TestExpireInlineTask_FlipsPendingToExpired covers the LRU-eviction
// cleanup path: when the cache evicts an inline-task hold, the
// runtime calls TasksHandler.ExpireInlineTask which must terminate
// the store.Task and resolve the canonical approval record so the
// dashboard stops showing a row whose chat anchor is gone.
func TestExpireInlineTask_FlipsPendingToExpired(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Build the thing",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}

	if err := h.ExpireInlineTask(ctx, taskID, agent.UserID); err != nil {
		t.Fatalf("ExpireInlineTask: %v", err)
	}

	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "expired" {
		t.Errorf("status=%q after ExpireInlineTask, want expired", got.Status)
	}

	// Canonical record resolved.
	recs, err := st.ListPendingApprovalRecords(ctx, agent.UserID)
	if err != nil {
		t.Fatalf("ListPendingApprovalRecords: %v", err)
	}
	for _, r := range recs {
		if r.TaskID != nil && *r.TaskID == taskID {
			t.Errorf("pending approval record should be resolved after ExpireInlineTask; still pending: %+v", r)
		}
	}
}

// TestExpireInlineTask_IdempotentOnTerminalRow confirms that calling
// ExpireInlineTask on a row already in a terminal state is a no-op
// success — important because eviction cleanup fires on every Hold
// commit and we don't want a benign race with the 24h sweep or a
// dashboard Deny to surface as an error.
func TestExpireInlineTask_IdempotentOnTerminalRow(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "X",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}
	// Drive to denied first.
	if err := h.DenyInlineTask(ctx, taskID, agent.UserID); err != nil {
		t.Fatalf("DenyInlineTask: %v", err)
	}
	// Now the eviction-triggered Expire should be a no-op success.
	if err := h.ExpireInlineTask(ctx, taskID, agent.UserID); err != nil {
		t.Fatalf("ExpireInlineTask on already-denied row: %v (want nil for idempotency)", err)
	}
}

// casLoserStore wraps a real store.Store and forces the
// UpdateTaskApprovedFrom CAS to lose. Subsequent GetTask calls
// (the re-fetch ApproveInlineTask issues after a lost CAS) return
// the task with status overridden to the configured terminalStatus,
// simulating the case where a concurrent dashboard Deny / expiry
// sweep / eviction landed BETWEEN the initial GetTask read and the
// CAS. Everything else passes through to the underlying store.
type casLoserStore struct {
	store.Store
	getCalls       int
	terminalStatus string
}

func (s *casLoserStore) UpdateTaskApprovedFrom(_ context.Context, _, _ string, _ time.Time, _ []store.TaskAction) (bool, error) {
	return false, nil
}

func (s *casLoserStore) GetTask(ctx context.Context, id string) (*store.Task, error) {
	task, err := s.Store.GetTask(ctx, id)
	if err != nil {
		return task, err
	}
	s.getCalls++
	// Second+ call to GetTask is the post-CAS-loss re-fetch; that's
	// where the concurrent terminal transition is visible.
	if s.getCalls >= 2 && task != nil {
		task.Status = s.terminalStatus
	}
	return task, nil
}

// TestApproveInlineTask_TerminalUpgradeOnLostCAS guards the lost-CAS
// path. The pre-CAS early terminal check catches the common case
// where the dashboard/sweep landed BEFORE the GetTask read, but a
// terminal transition can also land BETWEEN the read and the
// UpdateTaskApprovedFrom CAS. The CAS loser must still surface as
// a typed *ErrInlineTaskAlreadyTerminal so the chat reply renders
// "the user dismissed elsewhere; ask for a fresh request" instead
// of a generic creator-failure (which tells the model to acknowledge
// a failure without retrying — wrong UX when the user explicitly
// chose to dismiss).
//
// The casLoserStore wrapper forces the CAS to lose and makes the
// re-fetch observe a terminal status, exercising exactly the lost-
// CAS upgrade branch.
func TestApproveInlineTask_TerminalUpgradeOnLostCAS(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	req := &runtimetasks.TaskCreateRequest{
		Purpose:                "Build the thing",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools:          []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "Run"}},
	}
	taskID, err := h.CreatePendingInlineTask(ctx, agent, req, "tu-1", nil)
	if err != nil {
		t.Fatalf("CreatePendingInlineTask: %v", err)
	}

	// Swap the handler's store to the CAS-loser wrapper. The first
	// GetTask still sees pending_approval (early check passes), the
	// CAS loses, the re-fetch sees "expired" → typed error.
	h.st = &casLoserStore{Store: st, terminalStatus: "expired"}

	_, approveErr := h.ApproveInlineTask(ctx, taskID, agent.UserID)
	var terminal *llmproxy.ErrInlineTaskAlreadyTerminal
	if !errors.As(approveErr, &terminal) {
		t.Fatalf("ApproveInlineTask: err=%v (%T), want *llmproxy.ErrInlineTaskAlreadyTerminal", approveErr, approveErr)
	}
	if terminal.Status != "expired" {
		t.Errorf("terminal.Status=%q, want expired", terminal.Status)
	}
}

// TestApproveInlineTask_ReturnsAlreadyTerminalAfterDashboardDeny
// covers the race where the dashboard denied a chat-bound task and
// the model's "approve" reply arrives afterward. The chat path must
// surface a typed *llmproxy.ErrInlineTaskAlreadyTerminal so
// resolveInlineTaskApproval can render an "already dismissed
// elsewhere" reply instead of the generic creator-failed error.
func TestApproveInlineTask_ReturnsAlreadyTerminalAfterDashboardDeny(t *testing.T) {
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

	// Dashboard deny lands first.
	if err := h.DenyByTaskID(ctx, taskID, agent.UserID); err != nil {
		t.Fatalf("DenyByTaskID: %v", err)
	}

	// Chat-side approve arrives second — must surface the typed
	// terminal error.
	_, approveErr := h.ApproveInlineTask(ctx, taskID, agent.UserID)
	var terminal *llmproxy.ErrInlineTaskAlreadyTerminal
	if !errors.As(approveErr, &terminal) {
		t.Fatalf("ApproveInlineTask: err=%v (%T), want *llmproxy.ErrInlineTaskAlreadyTerminal", approveErr, approveErr)
	}
	if terminal.Status != "denied" {
		t.Errorf("terminal.Status=%q, want denied", terminal.Status)
	}

	// Task is still at denied — no spurious mutation from the
	// failed approve attempt.
	got, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "denied" {
		t.Errorf("status=%q after racing approve, want denied (unchanged)", got.Status)
	}
}

func anthropicInlineTaskPostBody(t *testing.T, taskBody string) []byte {
	t.Helper()
	if !json.Valid([]byte(taskBody)) {
		t.Fatalf("task body is not valid JSON: %s", taskBody)
	}
	input, err := json.Marshal(map[string]any{
		"url":    "https://clawvisor.local/control/tasks?wait=true&timeout=120&surface=inline",
		"method": "POST",
		"body":   json.RawMessage(taskBody),
	})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id":          "msg_inline_rollback",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-haiku-4-5",
		"stop_reason": "tool_use",
		"content": []map[string]any{{
			"type":  "tool_use",
			"id":    "toolu_inline_rollback",
			"name":  "Bash",
			"input": json.RawMessage(input),
		}},
	})
	if err != nil {
		t.Fatalf("marshal anthropic response: %v", err)
	}
	return body
}

type failingInlineTaskHoldCache struct {
	err       error
	holdCalls int
}

func (c *failingInlineTaskHoldCache) Hold(context.Context, llmproxy.PendingLiteApproval) (llmproxy.HoldResult, error) {
	c.holdCalls++
	return llmproxy.HoldResult{}, c.err
}

func (c *failingInlineTaskHoldCache) Peek(context.Context, llmproxy.ResolveRequest) (*llmproxy.PendingLiteApproval, error) {
	return nil, nil
}

func (c *failingInlineTaskHoldCache) Resolve(context.Context, llmproxy.ResolveRequest) (*llmproxy.PendingLiteApproval, error) {
	return nil, nil
}

func (c *failingInlineTaskHoldCache) Drop(context.Context, llmproxy.ResolveRequest) error {
	return nil
}

type evalThenErrorRewriter struct{}

func (evalThenErrorRewriter) Name() conversation.Provider { return conversation.ProviderAnthropic }

func (evalThenErrorRewriter) MatchesResponse(*http.Request, *http.Response) bool { return true }

func (evalThenErrorRewriter) Rewrite(body []byte, _ string, eval conversation.ToolUseEvaluator) (conversation.RewriteResult, error) {
	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return conversation.RewriteResult{}, err
	}
	for idx, block := range resp.Content {
		if block.Type != "tool_use" {
			continue
		}
		eval(conversation.ToolUse{
			ID:    block.ID,
			Index: idx,
			Name:  block.Name,
			Input: block.Input,
		})
	}
	return conversation.RewriteResult{}, errors.New("simulated rewriter failure after eval")
}
