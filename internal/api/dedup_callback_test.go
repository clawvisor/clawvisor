package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// registerCallbackSecret calls POST /api/callbacks/register and returns the secret.
func registerCallbackSecret(t *testing.T, env *testEnv, agentToken string) string {
	t.Helper()
	resp := env.do("POST", "/api/callbacks/register", agentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	secret, ok := body["callback_secret"].(string)
	if !ok || secret == "" {
		t.Fatal("registerCallbackSecret: expected non-empty callback_secret in response")
	}
	return secret
}

// ── request_id deduplication ──────────────────────────────────────────────────

func TestGateway_Dedup_BlockedRequest(t *testing.T) {
	// A blocked request, when resubmitted with the same request_id, should return
	// the existing outcome without creating a duplicate audit entry.
	env := newTestEnv(t)
	sc := newScenario(t, env, "dedup-block")
	sc.createRestriction(t, "mock.svc", "run", "blocked by test")

	reqID := fmt.Sprintf("dedup-blk-%s", randSuffix())

	first := sc.gatewayRequest(env, reqID, "mock.svc", "run")
	if first["status"] != "blocked" {
		t.Fatalf("first request: expected blocked, got %v", first["status"])
	}

	// Resubmit same request_id — dedup path should return cached outcome.
	second := sc.gatewayRequest(env, reqID, "mock.svc", "run")
	if second["status"] != "blocked" {
		t.Errorf("dedup: expected status=blocked (from cache), got %v", second["status"])
	}
	if second["deduped"] != true {
		t.Errorf("dedup: expected deduped=true, got %v", second["deduped"])
	}
	if second["message"] == nil || second["message"] == "" {
		t.Error("dedup: expected explanatory message on replayed response")
	}
	if second["request_id"] != reqID {
		t.Errorf("dedup: request_id mismatch: got %v", second["request_id"])
	}
	if second["audit_id"] != first["audit_id"] {
		t.Errorf("dedup: audit_id should match original: got %v, want %v", second["audit_id"], first["audit_id"])
	}

	// Exactly one audit entry should exist for this request_id.
	resp := sc.session.do("GET", "/api/audit", nil)
	body := mustStatus(t, resp, http.StatusOK)
	count := 0
	for _, e := range arr(t, body, "entries") {
		if e.(map[string]any)["request_id"] == reqID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedup: expected 1 audit entry, got %d", count)
	}
}

func TestGateway_Dedup_PendingRequest(t *testing.T) {
	// A pending (awaiting approval) request, when resubmitted, should return
	// status=pending without queueing a duplicate approval.
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "dedup-pend")

	taskID := sc.createApprovedTask(t, env, "mock.svc", "run", false)

	reqID := fmt.Sprintf("dedup-pend-%s", randSuffix())

	first := sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)
	if first["status"] != "pending" {
		t.Fatalf("first request: expected pending, got %v", first["status"])
	}

	second := sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)
	if second["status"] != "pending" {
		t.Errorf("dedup pending: expected status=pending, got %v", second["status"])
	}
	if second["deduped"] != true {
		t.Errorf("dedup pending: expected deduped=true, got %v", second["deduped"])
	}

	// Exactly one entry in the approvals queue (not two).
	resp := sc.session.do("GET", "/api/approvals", nil)
	apBody := mustStatus(t, resp, http.StatusOK)
	count := 0
	for _, e := range arr(t, apBody, "entries") {
		if e.(map[string]any)["request_id"] == reqID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedup pending: expected 1 approval entry, got %d", count)
	}
}

func TestGateway_Dedup_ExecutedRequest(t *testing.T) {
	// An already-executed request, when resubmitted with the same request_id,
	// should return status=executed from the audit log without re-running the adapter.
	adapter := newMockAdapter("mock.dedup-exec", "run").withResult("dedup-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "dedup-exec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.dedup-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.dedup-exec", "run", true)

	reqID := fmt.Sprintf("dedup-exec-%s", randSuffix())

	first := sc.gatewayRequestWithTask(env, reqID, "mock.dedup-exec", "run", taskID)
	if first["status"] != "executed" {
		t.Fatalf("first request: expected executed, got %v", first["status"])
	}

	second := sc.gatewayRequestWithTask(env, reqID, "mock.dedup-exec", "run", taskID)
	if second["status"] != "executed" {
		t.Errorf("dedup executed: expected status=executed, got %v", second["status"])
	}
	if second["deduped"] != true {
		t.Errorf("dedup executed: expected deduped=true, got %v", second["deduped"])
	}
	// The dedup path returns the same audit entry, so audit_id must match.
	if second["audit_id"] != first["audit_id"] {
		t.Errorf("dedup executed: audit_id should match original: got %v, want %v",
			second["audit_id"], first["audit_id"])
	}
}

func TestGateway_Dedup_DifferentRequestIDs_NotDeduplicated(t *testing.T) {
	// Two requests with different request_ids should each be processed independently.
	env := newTestEnv(t)
	sc := newScenario(t, env, "dedup-diff")
	sc.createRestriction(t, "mock.svc", "run", "blocked by test")

	r1 := sc.gatewayRequest(env, fmt.Sprintf("dedup-diff-a-%s", randSuffix()), "mock.svc", "run")
	r2 := sc.gatewayRequest(env, fmt.Sprintf("dedup-diff-b-%s", randSuffix()), "mock.svc", "run")

	if r1["audit_id"] == r2["audit_id"] {
		t.Error("different request_ids should produce different audit entries")
	}
}

func TestGateway_Dedup_SameRequestID_DifferentTask_NotDeduplicated(t *testing.T) {
	// A request_id reused under a different task should NOT be dedup'd.
	// This prevents stale audit entries from prior sessions leaking into new ones.
	adapter := newMockAdapter("mock.dedup-task", "run").withResult("fresh-result", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "dedup-task")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.dedup-task", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("dedup-xtask-%s", randSuffix())

	// First task: auto-execute → creates an "executed" audit entry.
	taskID1 := sc.createApprovedTask(t, env, "mock.dedup-task", "run", true)
	first := sc.gatewayRequestWithTask(env, reqID, "mock.dedup-task", "run", taskID1)
	if first["status"] != "executed" {
		t.Fatalf("first request: expected executed, got %v", first["status"])
	}

	// Second task (different purpose to avoid task content dedup) with the
	// same request_id — should NOT be dedup'd against the first task's entry.
	time.Sleep(10 * time.Millisecond) // ensure content dedup TTL separation
	sc.activateService(t, env, "mock.dedup-task")
	taskResp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "second task with different purpose",
		"authorized_actions": []map[string]any{{
			"service": "mock.dedup-task", "action": "run", "auto_execute": true,
		}},
	})
	taskBody := mustStatus(t, taskResp, http.StatusCreated)
	taskID2 := str(t, taskBody, "task_id")
	taskResp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID2), nil)
	mustStatus(t, taskResp, http.StatusOK)
	second := sc.gatewayRequestWithTask(env, reqID, "mock.dedup-task", "run", taskID2)
	if second["status"] != "executed" {
		t.Errorf("cross-task reuse: expected fresh executed, got %v", second["status"])
	}
	// Must be a NEW audit entry, not the cached one.
	if second["audit_id"] == first["audit_id"] {
		t.Error("cross-task reuse: should produce a new audit entry, got same audit_id")
	}
	// Fresh execution should include result data.
	if second["result"] == nil {
		t.Error("cross-task reuse: result should be present (not dedup cache)")
	}
}

// TestGateway_Dedup_AfterCrossTaskReuse_RetryHitsOwnCanonical guards the
// pre-LogAudit dedup check at gateway.go:264 against the cross-task-reuse
// shadowing bug. Sequence: task A executes reqID=X, task B (different task)
// executes reqID=X (independent canonical, allowed under symmetric scope),
// task A retries reqID=X. The pre-LogAudit check must dedup to task A's
// canonical, not task B's "latest" canonical — otherwise the retry falls
// through, the side effect re-fires, and LogAudit hits the partial-unique
// after the damage is done. FindDedupCandidate's same-task precedence is
// the only thing that prevents that.
func TestGateway_Dedup_AfterCrossTaskReuse_RetryHitsOwnCanonical(t *testing.T) {
	adapter := newMockAdapter("mock.crosstask-retry", "run").withResult("ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "crosstask-retry")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.crosstask-retry", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("crosstask-retry-%s", randSuffix())

	// Task A: auto-execute → canonical AC_A.
	taskA := sc.createApprovedTask(t, env, "mock.crosstask-retry", "run", true)
	first := sc.gatewayRequestWithTask(env, reqID, "mock.crosstask-retry", "run", taskA)
	if first["status"] != "executed" {
		t.Fatalf("task A first: expected executed, got %v", first["status"])
	}
	auditA := str(t, first, "audit_id")

	// Task B (different task, different purpose): also auto-execute, same
	// request_id. Under symmetric scope this lands its own canonical AC_B.
	time.Sleep(10 * time.Millisecond) // content-dedup TTL separation
	taskResp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "task B with different purpose",
		"authorized_actions": []map[string]any{{
			"service": "mock.crosstask-retry", "action": "run", "auto_execute": true,
		}},
	})
	taskBody := mustStatus(t, taskResp, http.StatusCreated)
	taskB := str(t, taskBody, "task_id")
	taskResp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskB), nil)
	mustStatus(t, taskResp, http.StatusOK)
	second := sc.gatewayRequestWithTask(env, reqID, "mock.crosstask-retry", "run", taskB)
	if second["status"] != "executed" {
		t.Fatalf("task B: expected executed, got %v", second["status"])
	}
	auditB := str(t, second, "audit_id")
	if auditA == auditB {
		t.Fatalf("task B should have its own canonical, got same audit_id as task A: %q", auditA)
	}

	// Task A retries reqID=X. Must dedup to AC_A (its own canonical), NOT
	// AC_B (the latest canonical for reqID=X). A request_id-only audit
	// lookup at the dedup gate would return AC_B and the retry would fall
	// through.
	retry := sc.gatewayRequestWithTask(env, reqID, "mock.crosstask-retry", "run", taskA)
	if retry["deduped"] != true {
		t.Fatalf("task A retry: expected deduped=true, got %v (full response: %+v)", retry["deduped"], retry)
	}
	if got := str(t, retry, "audit_id"); got != auditA {
		t.Fatalf("task A retry: should dedup to AC_A %q, got %q (AC_B was %q)", auditA, got, auditB)
	}
}

// TestGateway_Dedup_SameRequestID_DifferentTask_ApprovalRequired_IndependentApprovals
// pins the symmetric-dedup behavior change: when two tasks reuse the same
// request_id and both fall under approval-required, each task gets its own
// pending approval rather than the second being silently dedup'd to the first
// task's approval.
//
// Before symmetric scoping, pending_approvals.UNIQUE(request_id) forced the
// second insert to hit ErrConflict and the handler demoted the second audit
// row to a dedup-attempt pointing at the first task's pending. The
// user-visible effect was "two tasks share one approval queue entry," which
// is incoherent with the auto-execute case where each task gets an
// independent execution.
//
// After symmetric scoping, both inserts succeed under distinct
// (user_id, request_id, task_id) keys, so the queue shows two entries and
// denying one leaves the other pending.
func TestGateway_Dedup_SameRequestID_DifferentTask_ApprovalRequired_IndependentApprovals(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.crosstask-appr", "run"))
	sc := newScenario(t, env, "crosstask-appr")

	reqID := fmt.Sprintf("crosstask-appr-%s", randSuffix())

	// First task: approval-required → first pending approval.
	taskID1 := sc.createApprovedTask(t, env, "mock.crosstask-appr", "run", false)
	first := sc.gatewayRequestWithTask(env, reqID, "mock.crosstask-appr", "run", taskID1)
	if first["status"] != "pending" {
		t.Fatalf("first request: expected pending, got %v", first["status"])
	}
	firstAuditID := str(t, first, "audit_id")

	// Second task with a different purpose to avoid task-content dedup,
	// same request_id, also approval-required.
	taskResp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "second task with different purpose",
		"authorized_actions": []map[string]any{{
			"service": "mock.crosstask-appr", "action": "run", "auto_execute": false,
		}},
	})
	taskBody := mustStatus(t, taskResp, http.StatusCreated)
	taskID2 := str(t, taskBody, "task_id")
	taskResp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID2), nil)
	mustStatus(t, taskResp, http.StatusOK)
	second := sc.gatewayRequestWithTask(env, reqID, "mock.crosstask-appr", "run", taskID2)
	if second["status"] != "pending" {
		t.Fatalf("second request: expected pending, got %v", second["status"])
	}
	if second["deduped"] == true {
		t.Errorf("cross-task approval-required: second request should NOT be dedup'd, got deduped=true")
	}
	secondAuditID := str(t, second, "audit_id")
	if secondAuditID == firstAuditID {
		t.Errorf("cross-task approval-required: expected fresh audit_id, got same as first")
	}

	// /api/approvals should list two distinct pending entries, both with our
	// reqID, AND each must carry its task_id so the dashboard can resolve the
	// specific sibling via ?task_id= without hitting AMBIGUOUS.
	resp := sc.session.do("GET", "/api/approvals", nil)
	apBody := mustStatus(t, resp, http.StatusOK)
	gotTaskIDs := map[string]bool{}
	for _, e := range arr(t, apBody, "entries") {
		m := e.(map[string]any)
		if m["request_id"] != reqID {
			continue
		}
		taskID, ok := m["task_id"].(string)
		if !ok || taskID == "" {
			t.Fatalf("pending approval is missing task_id: %v", m)
		}
		gotTaskIDs[taskID] = true
	}
	if !gotTaskIDs[taskID1] || !gotTaskIDs[taskID2] {
		t.Fatalf("expected pending entries for both tasks, got task_ids %v (want %q + %q)", gotTaskIDs, taskID1, taskID2)
	}

	// Dashboard-style disambiguated approve via ?task_id= resolves the
	// specific sibling for each task.
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve?task_id=%s", reqID, taskID1), nil)
	mustStatus(t, resp, http.StatusOK)
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny?task_id=%s", reqID, taskID2), nil)
	mustStatus(t, resp, http.StatusOK)
}

// TestGateway_Status_TaskScoped guards the status / long-poll endpoint
// against sibling-task shadowing. Under symmetric dedup, two tasks can land
// distinct canonicals for the same request_id; the polling endpoint must
// return the row that matches the caller's task_id when supplied, not just
// "latest canonical for this request_id".
func TestGateway_Status_TaskScoped(t *testing.T) {
	adapter := newMockAdapter("mock.status-task", "run").withResult("ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "status-task")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.status-task", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	reqID := fmt.Sprintf("status-task-%s", randSuffix())

	taskA := sc.createApprovedTask(t, env, "mock.status-task", "run", true)
	first := sc.gatewayRequestWithTask(env, reqID, "mock.status-task", "run", taskA)
	if first["status"] != "executed" {
		t.Fatalf("task A: expected executed, got %v", first["status"])
	}
	auditA := str(t, first, "audit_id")

	// Task B also executes the same request_id (independent canonical under
	// symmetric scope). With ?task_id= each task's poll must resolve to that
	// task's own canonical, not whichever happens to be "latest". SQLite
	// stores audit timestamps at second granularity, so "latest" is unstable
	// for nearly-simultaneous rows — only the task-scoped contract is
	// deterministic enough to assert here.
	taskResp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "task B status scope",
		"authorized_actions": []map[string]any{{
			"service": "mock.status-task", "action": "run", "auto_execute": true,
		}},
	})
	taskBody := mustStatus(t, taskResp, http.StatusCreated)
	taskB := str(t, taskBody, "task_id")
	taskResp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskB), nil)
	mustStatus(t, taskResp, http.StatusOK)
	second := sc.gatewayRequestWithTask(env, reqID, "mock.status-task", "run", taskB)
	if second["status"] != "executed" {
		t.Fatalf("task B: expected executed, got %v", second["status"])
	}
	auditB := str(t, second, "audit_id")
	if auditA == auditB {
		t.Fatalf("expected distinct canonicals for two tasks, got same audit_id %q", auditA)
	}

	// Task-scoped status returns each task's own row regardless of which is
	// "newer" under second-granularity timestamps.
	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s?task_id=%s", reqID, taskA), sc.AgentToken, nil)
	got := mustStatus(t, resp, http.StatusOK)
	if got["audit_id"] != auditA {
		t.Fatalf("task-scoped status (A): expected %q, got %q", auditA, got["audit_id"])
	}
	resp = env.do("GET", fmt.Sprintf("/api/gateway/request/%s?task_id=%s", reqID, taskB), sc.AgentToken, nil)
	got = mustStatus(t, resp, http.StatusOK)
	if got["audit_id"] != auditB {
		t.Fatalf("task-scoped status (B): expected %q, got %q", auditB, got["audit_id"])
	}
}

// TestApprovals_Ambiguous_Returns409 covers the AMBIGUOUS disambiguation
// contract for the request_id-only HTTP routes. Two pending approvals share
// the same request_id (because they belong to different tasks); approving by
// request_id alone must surface 409 AMBIGUOUS with candidate_task_ids, and
// providing ?task_id= must resolve the correct row.
func TestApprovals_Ambiguous_Returns409(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.ambiguous", "run"))
	sc := newScenario(t, env, "ambiguous")

	reqID := fmt.Sprintf("ambig-%s", randSuffix())

	taskID1 := sc.createApprovedTask(t, env, "mock.ambiguous", "run", false)
	first := sc.gatewayRequestWithTask(env, reqID, "mock.ambiguous", "run", taskID1)
	if first["status"] != "pending" {
		t.Fatalf("first request: expected pending, got %v", first["status"])
	}

	taskResp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "second task ambiguous test",
		"authorized_actions": []map[string]any{{
			"service": "mock.ambiguous", "action": "run", "auto_execute": false,
		}},
	})
	taskBody := mustStatus(t, taskResp, http.StatusCreated)
	taskID2 := str(t, taskBody, "task_id")
	taskResp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID2), nil)
	mustStatus(t, taskResp, http.StatusOK)
	second := sc.gatewayRequestWithTask(env, reqID, "mock.ambiguous", "run", taskID2)
	if second["status"] != "pending" {
		t.Fatalf("second request: expected pending, got %v", second["status"])
	}

	// request_id-only approve hits 409 AMBIGUOUS with candidate task_ids.
	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	body := mustStatus(t, resp, http.StatusConflict)
	if body["code"] != "AMBIGUOUS" {
		t.Fatalf("expected code=AMBIGUOUS, got %v", body["code"])
	}
	candidates := arr(t, body, "candidate_task_ids")
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidate_task_ids, got %d", len(candidates))
	}
	gotTasks := map[string]bool{}
	for _, c := range candidates {
		gotTasks[c.(string)] = true
	}
	if !gotTasks[taskID1] || !gotTasks[taskID2] {
		t.Fatalf("candidate_task_ids missing one of %q/%q, got %v", taskID1, taskID2, candidates)
	}

	// Disambiguated approve via ?task_id= resolves the right row.
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve?task_id=%s", reqID, taskID1), nil)
	mustStatus(t, resp, http.StatusOK)

	// The other pending approval is still there; approve it explicitly.
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve?task_id=%s", reqID, taskID2), nil)
	mustStatus(t, resp, http.StatusOK)
}

// ── GET /api/gateway/request/{request_id} ────────────────────────────────────

func TestGateway_Status_NotFound(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "status-notfound")

	resp := env.do("GET", "/api/gateway/request/nonexistent-id", sc.AgentToken, nil)
	mustStatus(t, resp, http.StatusNotFound)
}

func TestGateway_Status_RequiresAgentToken(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do("GET", "/api/gateway/request/some-id", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestGateway_Status_Blocked(t *testing.T) {
	env := newTestEnv(t)
	sc := newScenario(t, env, "status-blk")
	sc.createRestriction(t, "mock.svc", "run", "blocked by test")

	reqID := fmt.Sprintf("status-blk-%s", randSuffix())
	sc.gatewayRequest(env, reqID, "mock.svc", "run")

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "blocked" {
		t.Errorf("status: expected blocked, got %v", body["status"])
	}
	if body["request_id"] != reqID {
		t.Errorf("status: request_id mismatch: got %v", body["request_id"])
	}
	if body["audit_id"] == nil || body["audit_id"] == "" {
		t.Error("status: audit_id missing")
	}
}

func TestGateway_Status_Pending(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "status-pend")

	taskID := sc.createApprovedTask(t, env, "mock.svc", "run", false)

	reqID := fmt.Sprintf("status-pend-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "pending" {
		t.Errorf("status: expected pending, got %v", body["status"])
	}
}

func TestGateway_Status_Executed(t *testing.T) {
	adapter := newMockAdapter("mock.status-exec", "run").withResult("status-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "status-exec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.status-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.status-exec", "run", true)

	reqID := fmt.Sprintf("status-exec-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.status-exec", "run", taskID)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Errorf("status: expected executed, got %v", body["status"])
	}
}

func TestGateway_Status_UpdatesAfterDeny(t *testing.T) {
	// A pending request, once denied, should show status=denied on the status endpoint.
	env := newTestEnv(t, newMockAdapter("mock.svc", "run"))
	sc := newScenario(t, env, "status-deny")

	taskID := sc.createApprovedTask(t, env, "mock.svc", "run", false)

	reqID := fmt.Sprintf("status-deny-%s", randSuffix())
	sc.gatewayRequestWithTask(env, reqID, "mock.svc", "run", taskID)

	sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)

	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc.AgentToken, nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "denied" {
		t.Errorf("status after deny: expected denied, got %v", body["status"])
	}
}

func TestGateway_Status_IsolatedByUser(t *testing.T) {
	// An agent belonging to user2 cannot poll the status of user1's request.
	env := newTestEnv(t)
	sc1 := newScenario(t, env, "status-iso1")
	sc1.createRestriction(t, "mock.svc", "run", "blocked for isolation test")

	reqID := fmt.Sprintf("status-iso-%s", randSuffix())
	sc1.gatewayRequest(env, reqID, "mock.svc", "run")

	sc2 := newScenario(t, env, "status-iso2")
	resp := env.do("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), sc2.AgentToken, nil)
	mustStatus(t, resp, http.StatusNotFound)
}

// ── Callback HMAC signing ─────────────────────────────────────────────────────

// callbackCapture records one received callback.
type callbackCapture struct {
	body []byte
	sig  string
}

// newCallbackServer starts a test HTTP server that records the first received POST.
func newCallbackServer(t *testing.T) (*httptest.Server, chan callbackCapture) {
	t.Helper()
	ch := make(chan callbackCapture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ch <- callbackCapture{body: body, sig: r.Header.Get("X-Clawvisor-Signature")}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// verifyCallbackHMAC asserts that sig == "sha256=" + HMAC-SHA256(body, key).
func verifyCallbackHMAC(t *testing.T, body []byte, sig, key, label string) {
	t.Helper()
	if sig == "" {
		t.Errorf("%s: X-Clawvisor-Signature header missing", label)
		return
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Errorf("%s: signature mismatch\n  got:  %s\n  want: %s", label, sig, want)
	}
}

func TestGateway_Callback_HMACSigned_OnExecute(t *testing.T) {
	// When a request is immediately executed via task, the callback POST must carry a valid
	// X-Clawvisor-Signature header, signed with the agent's registered callback secret.
	adapter := newMockAdapter("mock.cb-exec", "run").withResult("cb-exec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-exec")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-exec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	// Register callback secret
	cbSecret := registerCallbackSecret(t, env, sc.AgentToken)

	taskID := sc.createApprovedTask(t, env, "mock.cb-exec", "run", true)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-exec-%s", randSuffix())

	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-exec",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test callback",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Fatalf("expected executed, got %v", body["status"])
	}

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, cbSecret, "execute callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("callback: expected type=request, got %v", payload["type"])
		}
		if payload["request_id"] != reqID {
			t.Errorf("callback: request_id mismatch: got %v", payload["request_id"])
		}
		if payload["status"] != "executed" {
			t.Errorf("callback: expected status=executed, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("execute callback not received within 3s")
	}
}

func TestApprovals_Approve_CallbackHMACSigned(t *testing.T) {
	// When a pending request is approved, the callback must be HMAC-signed with
	// the agent's registered callback secret.
	adapter := newMockAdapter("mock.cb-approve", "run").withResult("approve-cb-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-approve")

	taskID := sc.createApprovedTask(t, env, "mock.cb-approve", "run", false)

	// Register callback secret
	cbSecret := registerCallbackSecret(t, env, sc.AgentToken)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-approve-%s", randSuffix())

	// Submit → pending (task has auto_execute=false)
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-approve",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test callback approve",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	firstBody := mustStatus(t, resp, http.StatusAccepted)
	if firstBody["status"] != "pending" {
		t.Fatalf("expected pending, got %v", firstBody["status"])
	}

	// Approve → triggers async callback
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, cbSecret, "approve callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("approve callback: expected type=request, got %v", payload["type"])
		}
		if payload["status"] != "approved" {
			t.Errorf("approve callback: expected status=approved, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approve callback not received within 3s")
	}
}

func TestApprovals_Deny_CallbackHMACSigned(t *testing.T) {
	// When a pending request is denied, the callback must be HMAC-signed.
	env := newTestEnv(t, newMockAdapter("mock.cb-deny", "run"))
	sc := newScenario(t, env, "cb-deny")

	taskID := sc.createApprovedTask(t, env, "mock.cb-deny", "run", false)

	// Register callback secret
	cbSecret := registerCallbackSecret(t, env, sc.AgentToken)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-deny-%s", randSuffix())

	// Submit → pending (task has auto_execute=false)
	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-deny",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test callback deny",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	firstBody := mustStatus(t, resp, http.StatusAccepted)
	if firstBody["status"] != "pending" {
		t.Fatalf("expected pending, got %v", firstBody["status"])
	}

	// Deny → triggers async callback
	resp = sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)
	mustStatus(t, resp, http.StatusOK)

	select {
	case cb := <-cbCh:
		verifyCallbackHMAC(t, cb.body, cb.sig, cbSecret, "deny callback")

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("deny callback: expected type=request, got %v", payload["type"])
		}
		if payload["status"] != "denied" {
			t.Errorf("deny callback: expected status=denied, got %v", payload["status"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deny callback not received within 3s")
	}
}

func TestGateway_Callback_NoCallbackURL_NoDelivery(t *testing.T) {
	// Requests without a callback_url should execute normally without any callback delivery.
	adapter := newMockAdapter("mock.cb-none", "run").withResult("no-cb-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "cb-none")
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-none", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.cb-none", "run", true)

	result := sc.gatewayRequestWithTask(env, fmt.Sprintf("cb-none-%s", randSuffix()), "mock.cb-none", "run", taskID)
	if result["status"] != "executed" {
		t.Errorf("no callback_url: expected status=executed, got %v", result["status"])
	}
}

// TestAgentCreate_DefaultsToSignedCallbacks asserts the default-on signing
// behavior: an agent created without setting with_callback_secret should
// receive a callback_secret in the response so callbacks are signed by
// default. This is the regression guard for the previous "default off"
// posture that left callbacks forgable to anyone learning the URL.
func TestAgentCreate_DefaultsToSignedCallbacks(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)
	resp := s.do("POST", "/api/agents", map[string]any{"name": "default-secret-test"})
	body := mustStatus(t, resp, http.StatusCreated)
	if _, ok := body["callback_secret"]; !ok {
		t.Fatal("expected callback_secret in agent-create response by default")
	}
}

func TestGateway_Callback_Unsigned_WhenNoSecret(t *testing.T) {
	// When an agent explicitly opts out (with_callback_secret=false at agent
	// creation) and no secret is registered after the fact, the callback
	// should still be delivered but with an empty X-Clawvisor-Signature
	// header. New agents now default to opt-in (signed callbacks); this
	// path is the regression guard for the explicit opt-out branch.
	adapter := newMockAdapter("mock.cb-nosec", "run").withResult("nosec-ok", nil)
	env := newTestEnv(t, adapter)
	sc := newScenarioOptingOutOfSignedCallbacks(t, env)
	if err := env.Vault.Set(context.Background(), sc.session.UserID, "mock.cb-nosec", []byte("cred")); err != nil {
		t.Fatalf("vault seed: %v", err)
	}

	taskID := sc.createApprovedTask(t, env, "mock.cb-nosec", "run", true)

	cbSrv, cbCh := newCallbackServer(t)
	reqID := fmt.Sprintf("cb-nosec-%s", randSuffix())

	resp := env.do("POST", "/api/gateway/request", sc.AgentToken, map[string]any{
		"service":    "mock.cb-nosec",
		"action":     "run",
		"params":     map[string]any{},
		"reason":     "test unsigned callback",
		"request_id": reqID,
		"task_id":    taskID,
		"context":    map[string]any{"callback_url": cbSrv.URL + "/inbound"},
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "executed" {
		t.Fatalf("expected executed, got %v", body["status"])
	}

	select {
	case cb := <-cbCh:
		// Callback should arrive but signature should be empty (unsigned)
		if cb.sig != "" {
			t.Errorf("unsigned callback: expected empty signature, got %q", cb.sig)
		}

		var payload map[string]any
		if err := json.Unmarshal(cb.body, &payload); err != nil {
			t.Fatalf("callback body not JSON: %v", err)
		}
		if payload["type"] != "request" {
			t.Errorf("callback: expected type=request, got %v", payload["type"])
		}
		if payload["request_id"] != reqID {
			t.Errorf("callback: request_id mismatch: got %v", payload["request_id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unsigned callback not received within 3s")
	}
}

func TestCallbackSecret_Register(t *testing.T) {
	// POST /api/callbacks/register should return a cbsec_-prefixed secret.
	env := newTestEnv(t)
	sc := newScenario(t, env, "cbsec-reg")

	secret := registerCallbackSecret(t, env, sc.AgentToken)
	if len(secret) < 10 || secret[:6] != "cbsec_" {
		t.Errorf("expected cbsec_-prefixed secret, got %q", secret)
	}

	// Calling again should return a different secret (rotation).
	secret2 := registerCallbackSecret(t, env, sc.AgentToken)
	if secret2 == secret {
		t.Error("expected rotated secret to differ from original")
	}
}

func TestCallbackSecret_RequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do("POST", "/api/callbacks/register", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}
