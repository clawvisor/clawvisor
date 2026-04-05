package smoke_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestGatewayReadOnly exercises the full gateway flow for a read-only
// action: create task → approve → gateway request → poll → execute.
func TestGatewayReadOnly(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	taskID := env.createApprovedTask(t, svcID, action, true)

	reqID := fmt.Sprintf("e2e-gw-%d", time.Now().UnixNano())
	resp := env.agentDo("POST", "/api/gateway/request", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     fmt.Sprintf("e2e smoke test — %s/%s", svcID, action),
		"request_id": reqID,
		"task_id":    taskID,
	})
	m := parseGatewayResponse(t, resp)

	status := str(t, m, "status")
	t.Logf("gateway response status: %s", status)

	env.executeIfReady(t, reqID, status, m)
}

// TestGatewayTaskApprovalFlow tests the full manual approval flow:
// create task with auto_execute=false, submit request, approve, execute.
func TestGatewayTaskApprovalFlow(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	// Create task WITHOUT auto_execute.
	taskID := env.createApprovedTask(t, svcID, action, false)

	reqID := fmt.Sprintf("e2e-gw-approval-%d", time.Now().UnixNano())
	resp := env.agentDo("POST", "/api/gateway/request", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     "e2e smoke test — approval flow",
		"request_id": reqID,
		"task_id":    taskID,
	})
	m := parseGatewayResponse(t, resp)
	status := str(t, m, "status")
	t.Logf("approval flow initial status: %s", status)

	if status == "pending" || status == "pending_approval" {
		approveResp := env.userDo("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
		defer approveResp.Body.Close()
		if approveResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(approveResp.Body)
			t.Fatalf("approve: status %d: %s", approveResp.StatusCode, body)
		}
		t.Log("approved request")

		env.pollAndExecute(t, reqID)
	} else {
		env.executeIfReady(t, reqID, status, m)
	}
}

// TestGatewayDeniedRequest tests that a denied request cannot be executed.
func TestGatewayDeniedRequest(t *testing.T) {
	env := setup(t)
	svcID, action, params := env.pickActivatedReadService(t)

	taskID := env.createApprovedTask(t, svcID, action, false)

	reqID := fmt.Sprintf("e2e-gw-deny-%d", time.Now().UnixNano())
	resp := env.agentDo("POST", "/api/gateway/request", map[string]any{
		"service":    svcID,
		"action":     action,
		"params":     params,
		"reason":     "e2e smoke test — deny flow",
		"request_id": reqID,
		"task_id":    taskID,
	})
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	// The gateway may return 200, 202, or 409 depending on task state timing.
	if resp.StatusCode == http.StatusConflict {
		t.Skipf("skipping deny test: gateway returned 409 (task state conflict): %s", b)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 200/202, got %d: %s", resp.StatusCode, b)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	status := str(t, m, "status")

	if status != "pending" && status != "pending_approval" {
		t.Skipf("skipping deny test: status was %s, not pending", status)
	}

	// Deny the request.
	denyResp := env.userDo("POST", fmt.Sprintf("/api/approvals/%s/deny", reqID), nil)
	defer denyResp.Body.Close()
	if denyResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(denyResp.Body)
		t.Fatalf("deny: status %d: %s", denyResp.StatusCode, body)
	}
	t.Log("denied request")

	// Verify execute fails.
	execResp := env.agentDo("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), nil)
	defer execResp.Body.Close()
	if execResp.StatusCode == http.StatusOK {
		t.Error("expected execute to fail after deny, but got 200")
	}
	t.Logf("execute after deny returned %d (expected non-200)", execResp.StatusCode)
}

// createApprovedTask creates a task with a single authorized action and
// approves it via the user token. Returns the task ID.
func (e *e2eEnv) createApprovedTask(t *testing.T, service, action string, autoExecute bool) string {
	t.Helper()

	resp := e.agentDo("POST", "/api/tasks", map[string]any{
		"purpose": fmt.Sprintf("e2e test: %s/%s", service, action),
		"authorized_actions": []map[string]any{{
			"service":      service,
			"action":       action,
			"auto_execute": autoExecute,
		}},
	})
	m := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, m, "task_id")

	// The task may need a moment for risk assessment to complete before
	// it enters "pending_approval" state. Poll the task until it's ready.
	for i := 0; i < 10; i++ {
		taskResp := e.agentDo("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
		taskM := mustStatus(t, taskResp, http.StatusOK)
		taskStatus := strOr(taskM, "status", "")
		if taskStatus == "pending_approval" || taskStatus == "pending" {
			break
		}
		if taskStatus == "approved" || taskStatus == "active" {
			t.Logf("task %s already %s (%s/%s auto_execute=%v)", taskID, taskStatus, service, action, autoExecute)
			return taskID
		}
		if taskStatus == "pending_scope_expansion" {
			// Left over from a prior test; approve the expansion first.
			expResp := e.userDo("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
			expResp.Body.Close()
			continue
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Approve the task.
	approveResp := e.userDo("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	defer approveResp.Body.Close()
	approveBody, _ := io.ReadAll(approveResp.Body)
	if approveResp.StatusCode != http.StatusOK {
		// Task may have been auto-approved already; check if it's in a usable state.
		taskResp := e.agentDo("GET", fmt.Sprintf("/api/tasks/%s", taskID), nil)
		taskM := mustStatus(t, taskResp, http.StatusOK)
		taskStatus := strOr(taskM, "status", "")
		if taskStatus == "approved" || taskStatus == "active" {
			t.Logf("task %s already %s (approve returned %d)", taskID, taskStatus, approveResp.StatusCode)
			return taskID
		}
		t.Fatalf("approve task %s: status %d: %s (task status: %s)", taskID, approveResp.StatusCode, approveBody, taskStatus)
	}
	t.Logf("created and approved task %s (%s/%s auto_execute=%v)", taskID, service, action, autoExecute)
	return taskID
}

// parseGatewayResponse handles both 200 and 202 responses from the gateway.
func parseGatewayResponse(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected HTTP 200 or 202, got %d: %s", resp.StatusCode, b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode (status=%d body=%s): %v", resp.StatusCode, b, err)
	}
	return m
}

// executeIfReady handles the common pattern of executing a gateway request
// based on its status.
func (e *e2eEnv) executeIfReady(t *testing.T, reqID, status string, m map[string]any) {
	t.Helper()

	switch status {
	case "approved", "auto_approved":
		execResp := e.agentDo("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), nil)
		execM := mustStatus(t, execResp, http.StatusOK)
		t.Logf("execute result: summary=%v", execM["summary"])
	case "completed", "executed":
		t.Logf("already executed: summary=%v", m["summary"])
	case "pending", "pending_approval":
		// Approve then execute.
		approveResp := e.userDo("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
		defer approveResp.Body.Close()
		if approveResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(approveResp.Body)
			t.Fatalf("approve: status %d: %s", approveResp.StatusCode, body)
		}
		e.pollAndExecute(t, reqID)
	case "restricted":
		// Intent verification flagged the request — not a test failure.
		t.Logf("request restricted by intent verification (status=restricted)")
	case "blocked":
		t.Logf("request blocked by restriction rule (status=blocked)")
	case "error":
		errMsg := strOr(m, "error", strOr(m, "message", "unknown error"))
		t.Fatalf("gateway execution error: %s", errMsg)
	default:
		t.Fatalf("unexpected status: %s (response: %v)", status, m)
	}
}

// pollAndExecute polls for an approved gateway request and executes it.
func (e *e2eEnv) pollAndExecute(t *testing.T, reqID string) {
	t.Helper()

	// Poll until the request is approved/ready.
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		pollResp := e.agentDo("GET", fmt.Sprintf("/api/gateway/request/%s", reqID), nil)
		pollM := mustStatus(t, pollResp, http.StatusOK)
		s := str(t, pollM, "status")
		if s == "approved" || s == "auto_approved" {
			execResp := e.agentDo("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), nil)
			execM := mustStatus(t, execResp, http.StatusOK)
			t.Logf("execute result: summary=%v", execM["summary"])
			return
		}
		if s == "executed" || s == "completed" {
			t.Logf("already executed: %v", pollM["summary"])
			return
		}
		if s == "denied" || s == "expired" || s == "failed" {
			t.Fatalf("request ended with status: %s", s)
		}
	}
	t.Fatal("timed out waiting for request approval")
}
