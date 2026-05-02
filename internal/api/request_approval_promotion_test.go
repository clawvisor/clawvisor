package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestRequestApprovalAllowSessionPromotesToActiveTask(t *testing.T) {
	adapter := newMockAdapter("mock.promote-session", "run").
		withResult("ok", map[string]any{"status": "done"})
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "promote-session")

	taskID := sc.createApprovedTask(t, env, "mock.promote-session", "run", false)
	reqID := fmt.Sprintf("req-promote-session-%s", randSuffix())

	result := sc.gatewayRequestWithTask(env, reqID, "mock.promote-session", "run", taskID)
	if result["status"] != "pending" {
		t.Fatalf("expected pending approval, got %v", result["status"])
	}

	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), map[string]any{
		"resolution": "allow_session",
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["status"] != "approved" {
		t.Fatalf("expected approved status, got %v", body["status"])
	}
	if body["resolution"] != "allow_session" {
		t.Fatalf("expected allow_session resolution, got %v", body["resolution"])
	}
	promotedTaskID := str(t, body, "task_id")

	promotedTask, err := env.Store.GetTask(context.Background(), promotedTaskID)
	if err != nil {
		t.Fatalf("GetTask(promoted): %v", err)
	}
	if promotedTask.Status != "active" {
		t.Fatalf("expected promoted task active, got %q", promotedTask.Status)
	}
	if promotedTask.Lifetime != "session" {
		t.Fatalf("expected promoted task session lifetime, got %q", promotedTask.Lifetime)
	}
	if promotedTask.ExpiresAt == nil {
		t.Fatal("expected session promoted task expiry to be set")
	}
	if len(promotedTask.AuthorizedActions) != 1 {
		t.Fatalf("expected one authorized action, got %d", len(promotedTask.AuthorizedActions))
	}
	if promotedTask.AuthorizedActions[0].Service != "mock.promote-session" || promotedTask.AuthorizedActions[0].Action != "run" {
		t.Fatalf("unexpected promoted task scope: %+v", promotedTask.AuthorizedActions[0])
	}
	if !promotedTask.AuthorizedActions[0].AutoExecute {
		t.Fatal("expected promoted task scope to auto-execute")
	}

	rec, err := env.Store.GetApprovalRecordByRequestID(context.Background(), reqID)
	if err != nil {
		t.Fatalf("GetApprovalRecordByRequestID: %v", err)
	}
	if rec.Resolution != "allow_session" {
		t.Fatalf("expected canonical approval resolution allow_session, got %q", rec.Resolution)
	}

	resp = env.do("POST", fmt.Sprintf("/api/gateway/request/%s/execute", reqID), sc.AgentToken, nil)
	executed := mustStatus(t, resp, http.StatusOK)
	if executed["status"] != "executed" {
		t.Fatalf("expected executed status after approval, got %v", executed["status"])
	}
}

func TestRequestApprovalAllowAlwaysPromotesToStandingTask(t *testing.T) {
	adapter := newMockAdapter("mock.promote-standing", "run").
		withResult("ok", map[string]any{"status": "done"})
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "promote-standing")

	taskID := sc.createApprovedTask(t, env, "mock.promote-standing", "run", false)
	reqID := fmt.Sprintf("req-promote-standing-%s", randSuffix())

	result := sc.gatewayRequestWithTask(env, reqID, "mock.promote-standing", "run", taskID)
	if result["status"] != "pending" {
		t.Fatalf("expected pending approval, got %v", result["status"])
	}

	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), map[string]any{
		"resolution": "allow_always",
	})
	body := mustStatus(t, resp, http.StatusOK)
	if body["resolution"] != "allow_always" {
		t.Fatalf("expected allow_always resolution, got %v", body["resolution"])
	}
	promotedTaskID := str(t, body, "task_id")

	promotedTask, err := env.Store.GetTask(context.Background(), promotedTaskID)
	if err != nil {
		t.Fatalf("GetTask(promoted): %v", err)
	}
	if promotedTask.Status != "active" {
		t.Fatalf("expected promoted task active, got %q", promotedTask.Status)
	}
	if promotedTask.Lifetime != "standing" {
		t.Fatalf("expected promoted task standing lifetime, got %q", promotedTask.Lifetime)
	}
	if promotedTask.ExpiresAt != nil {
		t.Fatalf("expected no expiry for standing promoted task, got %v", promotedTask.ExpiresAt)
	}

	rec, err := env.Store.GetApprovalRecordByRequestID(context.Background(), reqID)
	if err != nil {
		t.Fatalf("GetApprovalRecordByRequestID: %v", err)
	}
	if rec.Resolution != "allow_always" {
		t.Fatalf("expected canonical approval resolution allow_always, got %q", rec.Resolution)
	}
}

func TestRequestApprovalDefaultRemainsAllowOnce(t *testing.T) {
	adapter := newMockAdapter("mock.promote-default", "run")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "promote-default")

	taskID := sc.createApprovedTask(t, env, "mock.promote-default", "run", false)
	reqID := fmt.Sprintf("req-promote-default-%s", randSuffix())

	result := sc.gatewayRequestWithTask(env, reqID, "mock.promote-default", "run", taskID)
	if result["status"] != "pending" {
		t.Fatalf("expected pending approval, got %v", result["status"])
	}

	resp := sc.session.do("POST", fmt.Sprintf("/api/approvals/%s/approve", reqID), nil)
	body := mustStatus(t, resp, http.StatusOK)
	if body["resolution"] != "allow_once" {
		t.Fatalf("expected default resolution allow_once, got %v", body["resolution"])
	}
	if _, ok := body["task_id"]; ok {
		t.Fatalf("expected no promoted task for allow_once, got %v", body["task_id"])
	}

	rec, err := env.Store.GetApprovalRecordByRequestID(context.Background(), reqID)
	if err != nil {
		t.Fatalf("GetApprovalRecordByRequestID: %v", err)
	}
	if rec.Resolution != "allow_once" {
		t.Fatalf("expected canonical approval resolution allow_once, got %q", rec.Resolution)
	}
}
