package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// seedActiveTask creates an inline-approved active task owned by the
// given agent and returns its row. Mirrors the setup the other tests
// use so we exercise the same approval path that produces tasks in
// production.
func seedActiveTask(t *testing.T, h *TasksHandler, agent *store.Agent, purpose string) *store.Task {
	t.Helper()
	ctx := context.Background()
	req := &runtimetasks.TaskCreateRequest{
		Purpose: purpose,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "Test command"},
		},
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
	}
	out, err := h.CreateInlineApprovedTask(ctx, agent, req, "cv-test-tooluse-"+strings.ReplaceAll(purpose, " ", "-"))
	if err != nil {
		t.Fatalf("CreateInlineApprovedTask: %v", err)
	}
	task, err := h.st.GetTask(ctx, out.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

func newCompleteRequest(taskID string, agent *store.Agent) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/complete", nil)
	req.SetPathValue("id", taskID)
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	return req
}

func TestComplete_HappyPath_ActiveTask(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	task := seedActiveTask(t, h, agent, "rename foo to bar")

	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agent))

	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if resp["task_id"] != task.ID || resp["status"] != "completed" {
		t.Errorf("response = %v, want {task_id:%q, status:completed}", resp, task.ID)
	}
	persisted, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if persisted.Status != "completed" {
		t.Errorf("persisted status = %q, want completed", persisted.Status)
	}
}

// TestComplete_HappyPath_ExpiredTask covers the "expired but not yet
// completed" case: the timer ticked over before the agent could call
// complete. The handler still accepts it so chain-fact cleanup runs —
// without this the closed-task facts linger until the GC sweep.
func TestComplete_HappyPath_ExpiredTask(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()
	task := seedActiveTask(t, h, agent, "expired task")

	// Force the row to expired. Done out-of-band to avoid coupling to
	// the expiration sweeper's clock.
	if won, err := st.UpdateTaskStatusFrom(ctx, task.ID, "active", "expired"); err != nil || !won {
		t.Fatalf("setup: flip task to expired: won=%v err=%v", won, err)
	}

	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agent))

	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	persisted, _ := st.GetTask(ctx, task.ID)
	if persisted.Status != "completed" {
		t.Errorf("persisted status = %q, want completed (expired → completed must succeed)", persisted.Status)
	}
}

func TestComplete_AlreadyCompleted_Returns409(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	task := seedActiveTask(t, h, agent, "double-complete")

	// First complete: succeeds.
	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agent))
	if rec.Code != http.StatusOK {
		t.Fatalf("first Complete status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Second complete: 409 INVALID_STATE — task is not active/expired.
	rec2 := httptest.NewRecorder()
	h.Complete(rec2, newCompleteRequest(task.ID, agent))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second Complete status = %d, want 409; body = %s", rec2.Code, rec2.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp)
	if resp["code"] != "INVALID_STATE" {
		t.Errorf("code = %v, want INVALID_STATE", resp["code"])
	}
}

// TestComplete_PendingScopeExpansion_LosesCAS is the regression bar
// for the CAS fix: a task that flipped to pending_scope_expansion
// between the preflight read and the write must NOT be silently
// overwritten with "completed".
//
// We can't actually race the read/write in a unit test, so we simulate
// the racing branch by driving the row to pending_scope_expansion
// before calling Complete. The preflight check at status != active &&
// status != expired catches it first; the CAS branch is reached only
// when the row was active at preflight but flipped before the write.
// Either path returns 409 — that's the contract.
func TestComplete_PendingScopeExpansion_Returns409(t *testing.T) {
	h, st, _, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()
	task := seedActiveTask(t, h, agent, "expand-then-complete")

	if won, err := st.UpdateTaskStatusFrom(ctx, task.ID, "active", "pending_scope_expansion"); err != nil || !won {
		t.Fatalf("setup: flip task to pending_scope_expansion: won=%v err=%v", won, err)
	}

	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agent))
	if rec.Code != http.StatusConflict {
		t.Fatalf("Complete status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "pending_scope_expansion") {
		t.Errorf("error message should surface the live status so the agent can react; got %q", errMsg)
	}

	// Critical: the row's status must NOT have been clobbered.
	persisted, _ := st.GetTask(ctx, task.ID)
	if persisted.Status != "pending_scope_expansion" {
		t.Fatalf("blind status overwrite regression: status = %q, want pending_scope_expansion preserved", persisted.Status)
	}
}

// TestComplete_RevokedTask_Returns409 covers the user-revocation race.
// A task the user explicitly revoked must NOT be overwritten by a
// concurrent agent complete — the revoke decision is authoritative.
func TestComplete_RevokedTask_Returns409(t *testing.T) {
	h, st, user, agent := newInlineTasksHandlerForTest(t)
	ctx := context.Background()
	task := seedActiveTask(t, h, agent, "revoke-then-complete")

	if err := st.RevokeTask(ctx, task.ID, user.ID); err != nil {
		t.Fatalf("setup: RevokeTask: %v", err)
	}

	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agent))
	if rec.Code != http.StatusConflict {
		t.Fatalf("Complete status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	persisted, _ := st.GetTask(ctx, task.ID)
	if persisted.Status != "revoked" {
		t.Fatalf("revoke overwrite regression: status = %q, want revoked preserved", persisted.Status)
	}
}

// TestComplete_CrossAgent_Returns403 is the regression bar for the
// task.AgentID pin. Before this change Complete only checked UserID,
// so a sibling agent under the same user could close out (and delete
// chain facts for) a task it did not create.
func TestComplete_CrossAgent_Returns403(t *testing.T) {
	h, st, user, agentA := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	agentB, err := st.CreateAgent(ctx, user.ID, "sibling-agent", "token-hash-b")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	task := seedActiveTask(t, h, agentA, "agentA's task")

	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agentB))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Complete status = %d, want 403 (cross-agent); body = %s", rec.Code, rec.Body.String())
	}

	// Task must remain active — the wrong agent's complete must not
	// have any side effect on the row.
	persisted, _ := st.GetTask(ctx, task.ID)
	if persisted.Status != "active" {
		t.Errorf("cross-agent complete had side effect: status = %q, want active", persisted.Status)
	}
}

func TestComplete_CrossUser_Returns403(t *testing.T) {
	h, st, _, agentA := newInlineTasksHandlerForTest(t)
	ctx := context.Background()

	otherUser, err := st.CreateUser(ctx, "other-user@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agentB, err := st.CreateAgent(ctx, otherUser.ID, "other-user-agent", "other-token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	task := seedActiveTask(t, h, agentA, "cross-user")
	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agentB))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Complete status = %d, want 403 (cross-user); body = %s", rec.Code, rec.Body.String())
	}
}

func TestComplete_UnknownTask_Returns404(t *testing.T) {
	h, _, _, agent := newInlineTasksHandlerForTest(t)
	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest("does-not-exist", agent))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Complete status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// flipActiveToExpiredOnFirstCASStore is a store wrapper that
// deterministically reproduces the active → expired race between the
// Complete handler's preflight read and its CAS write. On the first
// "active → completed" CAS, it flips the underlying row to expired
// and returns won=false; subsequent CAS calls pass through to the
// real store unchanged.
type flipActiveToExpiredOnFirstCASStore struct {
	store.Store
	mu      sync.Mutex
	flipped bool
}

func (w *flipActiveToExpiredOnFirstCASStore) UpdateTaskStatusFrom(ctx context.Context, id, from, to string) (bool, error) {
	w.mu.Lock()
	intercept := !w.flipped && from == "active" && to == "completed"
	if intercept {
		w.flipped = true
	}
	w.mu.Unlock()
	if intercept {
		// Simulate the expiration sweeper flipping the row mid-flight.
		// The underlying CAS still uses the real store so the row
		// actually moves to expired — the handler's next GetTask will
		// observe that on the retry path.
		if _, err := w.Store.UpdateTaskStatusFrom(ctx, id, "active", "expired"); err != nil {
			return false, err
		}
		return false, nil
	}
	return w.Store.UpdateTaskStatusFrom(ctx, id, from, to)
}

// TestComplete_ActiveToExpiredRace_RetriesAndSucceeds locks in the
// fix for the active→expired benign race. Before the retry, this
// scenario would 409 — the preflight accepted active, the CAS pinned
// fromStatus=active, the expiration sweeper raced in between, and the
// CAS lost despite expired being an equally-valid pre-complete state.
// Worse, chain-fact cleanup also got skipped.
//
// The fix: on CAS loss, re-read the live status; if it's still
// completable (active or expired), retry the CAS once with the live
// fromStatus.
func TestComplete_ActiveToExpiredRace_RetriesAndSucceeds(t *testing.T) {
	h, realStore, _, agent := newInlineTasksHandlerForTest(t)
	task := seedActiveTask(t, h, agent, "race-active-to-expired")

	// Swap in the wrapper for the Complete call only. Seeding through
	// the real store first avoids interference with task creation,
	// which also exercises UpdateTaskStatusFrom under the hood.
	h.st = &flipActiveToExpiredOnFirstCASStore{Store: realStore}

	rec := httptest.NewRecorder()
	h.Complete(rec, newCompleteRequest(task.ID, agent))

	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status = %d, want 200 (active→expired race must NOT surface as 409); body = %s", rec.Code, rec.Body.String())
	}
	persisted, err := realStore.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if persisted.Status != "completed" {
		t.Errorf("persisted status = %q, want completed", persisted.Status)
	}
}

func TestComplete_Unauthenticated_Returns401(t *testing.T) {
	h, _, _, _ := newInlineTasksHandlerForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/some-id/complete", nil)
	req.SetPathValue("id", "some-id")
	// Intentionally no store.WithAgent.

	rec := httptest.NewRecorder()
	h.Complete(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Complete status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}
