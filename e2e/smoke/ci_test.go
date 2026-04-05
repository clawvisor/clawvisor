package smoke_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── Service Activation Tests ─────────────────────────────────────────────────

// TestCIOAuthActivation tests the full OAuth dance: activate → redirect →
// callback → token exchange → credential stored → service activated.
func TestCIOAuthActivation(t *testing.T) {
	env := ciSetup(t)
	env.activateTestOAuth(t)
}

// TestCIAPIKeyActivation tests API key activation: POST token → validate →
// vault store → service activated.
func TestCIAPIKeyActivation(t *testing.T) {
	env := ciSetup(t)
	env.activateTestAPIKey(t)
}

// TestCINoAuthActivation tests credential-free service activation:
// POST activate → service_meta created → service activated.
func TestCINoAuthActivation(t *testing.T) {
	env := ciSetup(t)
	env.activateTestNoAuth(t)
}

// ── Gateway Flow Tests ───────────────────────────────────────────────────────

// TestCIOAuthGatewayFlow runs the full gateway pipeline through the
// OAuth-authenticated test service: task → approve → gateway request → execute.
func TestCIOAuthGatewayFlow(t *testing.T) {
	env := ciSetup(t)
	env.activateTestOAuth(t)
	env.runGatewayFlow(t, "test_oauth", "list_items")
}

// TestCIAPIKeyGatewayFlow runs the full gateway pipeline through the
// API key-authenticated test service.
func TestCIAPIKeyGatewayFlow(t *testing.T) {
	env := ciSetup(t)
	env.activateTestAPIKey(t)
	env.runGatewayFlow(t, "test_apikey", "list_items")
}

// TestCINoAuthGatewayFlow runs the full gateway pipeline through the
// credential-free test service.
func TestCINoAuthGatewayFlow(t *testing.T) {
	env := ciSetup(t)
	env.activateTestNoAuth(t)
	env.runGatewayFlow(t, "test_noauth", "list_items")
}

// ── Full Agent Lifecycle with Test Adapters ──────────────────────────────────

// TestCIAgentLifecycle exercises the complete agent lifecycle using test
// adapters: connect → catalog → task → gateway → complete.
func TestCIAgentLifecycle(t *testing.T) {
	env := ciSetup(t)

	// Activate all test services.
	env.activateTestOAuth(t)
	env.activateTestAPIKey(t)
	env.activateTestNoAuth(t)

	// ── Connect a new agent with ?wait=true ──────────────────────────────
	agentName := fmt.Sprintf("ci-lifecycle-%d", time.Now().UnixNano())
	approvedCh := approveConnectionInBackground(t, env.e2eEnv, agentName)

	connResp := env.doLongPoll("POST", "/api/agents/connect?wait=true&timeout=30", "", map[string]any{
		"name":        agentName,
		"description": "CI lifecycle test agent",
	})
	connM := mustStatus(t, connResp, http.StatusCreated)
	if strOr(connM, "status", "") != "approved" {
		t.Fatalf("expected approved, got %s", strOr(connM, "status", ""))
	}
	agentToken := strOr(connM, "token", "")
	if agentToken == "" || !strings.HasPrefix(agentToken, "cvis_") {
		t.Fatal("expected cvis_ token")
	}
	t.Logf("connected as %s", agentName)
	<-approvedCh

	agentReq := func(method, path string, body any) *http.Response {
		return env.doRaw(method, path, agentToken, body)
	}
	agentLongPoll := func(method, path string, body any) *http.Response {
		return env.doLongPoll(method, path, agentToken, body)
	}

	// ── Fetch catalog ────────────────────────────────────────────────────
	catalogResp := agentReq("GET", "/api/skill/catalog", nil)
	defer catalogResp.Body.Close()
	if catalogResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(catalogResp.Body)
		t.Fatalf("catalog: status %d: %s", catalogResp.StatusCode, body)
	}
	catalogBody, _ := io.ReadAll(catalogResp.Body)
	catalog := string(catalogBody)

	// All three test services should appear.
	for _, svc := range []string{"test_oauth", "test_apikey", "test_noauth"} {
		if !strings.Contains(catalog, svc) {
			t.Errorf("catalog missing %s", svc)
		}
	}
	t.Logf("catalog: %d bytes, all test services found", len(catalog))

	// ── Test each service through the gateway ────────────────────────────
	for _, tc := range []struct {
		service string
		action  string
	}{
		{"test_oauth", "list_items"},
		{"test_apikey", "list_items"},
		{"test_noauth", "list_items"},
	} {
		t.Run(tc.service, func(t *testing.T) {
			purposeMarker := fmt.Sprintf("ci-lifecycle-%s-%d", tc.service, time.Now().UnixNano())
			taskApprovedCh := approveTaskInBackground(t, env.e2eEnv, purposeMarker)

			taskResp := agentLongPoll("POST", "/api/tasks?wait=true&timeout=30", map[string]any{
				"purpose": fmt.Sprintf("CI lifecycle [%s]: %s/%s", purposeMarker, tc.service, tc.action),
				"authorized_actions": []map[string]any{{
					"service":      tc.service,
					"action":       tc.action,
					"auto_execute": true,
				}},
			})
			taskM := mustStatus(t, taskResp, http.StatusCreated)
			taskID := strOr(taskM, "id", strOr(taskM, "task_id", ""))
			if taskID == "" {
				t.Fatal("no task_id")
			}
			taskStatus := strOr(taskM, "status", "")
			if taskStatus != "approved" && taskStatus != "active" {
				t.Fatalf("expected approved/active, got %s", taskStatus)
			}
			<-taskApprovedCh

			reqID := fmt.Sprintf("ci-%s-%d", tc.service, time.Now().UnixNano())
			gwResp := agentLongPoll("POST", "/api/gateway/request?wait=true&timeout=30", map[string]any{
				"service":    tc.service,
				"action":     tc.action,
				"params":     map[string]any{},
				"reason":     fmt.Sprintf("CI lifecycle test: %s/%s", tc.service, tc.action),
				"request_id": reqID,
				"task_id":    taskID,
			})
			gwM := parseGatewayResponse(t, gwResp)
			gwStatus := str(t, gwM, "status")
			t.Logf("gateway status=%s", gwStatus)
			env.executeIfReady(t, reqID, gwStatus, gwM)

			completeResp := agentReq("POST", fmt.Sprintf("/api/tasks/%s/complete", taskID), nil)
			defer completeResp.Body.Close()
			if completeResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(completeResp.Body)
				t.Fatalf("complete: status %d: %s", completeResp.StatusCode, body)
			}
			t.Logf("task %s completed", taskID)
		})
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// runGatewayFlow creates a task, approves it, sends a gateway request, and
// verifies execution through the full pipeline for a single service/action.
func (e *ciTestEnv) runGatewayFlow(t *testing.T, service, action string) {
	t.Helper()

	purposeMarker := fmt.Sprintf("ci-gw-%s-%d", service, time.Now().UnixNano())
	taskApprovedCh := approveTaskInBackground(t, e.e2eEnv, purposeMarker)

	taskResp := e.doLongPoll("POST", "/api/tasks?wait=true&timeout=30", e.AgentToken, map[string]any{
		"purpose": fmt.Sprintf("CI gateway test [%s]: %s/%s", purposeMarker, service, action),
		"authorized_actions": []map[string]any{{
			"service":      service,
			"action":       action,
			"auto_execute": true,
		}},
	})
	taskM := mustStatus(t, taskResp, http.StatusCreated)
	taskID := strOr(taskM, "id", strOr(taskM, "task_id", ""))
	if taskID == "" {
		t.Fatal("no task_id")
	}
	taskStatus := strOr(taskM, "status", "")
	if taskStatus != "approved" && taskStatus != "active" {
		t.Fatalf("expected approved/active, got %s", taskStatus)
	}
	t.Logf("task %s approved", taskID)
	<-taskApprovedCh

	reqID := fmt.Sprintf("ci-gw-%s-%d", service, time.Now().UnixNano())
	gwResp := e.doLongPoll("POST", "/api/gateway/request?wait=true&timeout=30", e.AgentToken, map[string]any{
		"service":    service,
		"action":     action,
		"params":     map[string]any{},
		"reason":     fmt.Sprintf("CI gateway test: %s/%s", service, action),
		"request_id": reqID,
		"task_id":    taskID,
	})
	gwM := parseGatewayResponse(t, gwResp)
	gwStatus := str(t, gwM, "status")
	t.Logf("gateway status=%s", gwStatus)
	e.executeIfReady(t, reqID, gwStatus, gwM)

	completeResp := e.agentDo("POST", fmt.Sprintf("/api/tasks/%s/complete", taskID), nil)
	defer completeResp.Body.Close()
	if completeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(completeResp.Body)
		t.Fatalf("complete: status %d: %s", completeResp.StatusCode, body)
	}
	t.Log("task completed — gateway flow passed")
}
