package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestTaskCreateApprovalRecordsResolveCanonically(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.task-approval", "read"))
	sc := newScenario(t, env, "task-approval")
	sc.activateService(t, env, "mock.task-approval")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "review account activity",
		"authorized_actions": []map[string]any{{
			"service": "mock.task-approval", "action": "read", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	rec := mustPendingTaskApprovalRecord(t, env.Store, sc.session.UserID, taskID, "task_create")
	if rec.Status != "pending" {
		t.Fatalf("expected pending task approval record, got %q", rec.Status)
	}

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resolved, err := env.Store.GetApprovalRecord(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if resolved.Status != "approved" {
		t.Fatalf("expected approved status, got %q", resolved.Status)
	}
	if resolved.Resolution != "allow_session" {
		t.Fatalf("expected allow_session resolution, got %q", resolved.Resolution)
	}
}

func TestStandingTaskCreateApprovalRecordResolvesAllowAlways(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.task-standing", "read"))
	sc := newScenario(t, env, "task-standing")
	sc.activateService(t, env, "mock.task-standing")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose":  "keep monitoring account activity",
		"lifetime": "standing",
		"authorized_actions": []map[string]any{{
			"service": "mock.task-standing", "action": "read", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	rec := mustPendingTaskApprovalRecord(t, env.Store, sc.session.UserID, taskID, "task_create")

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resolved, err := env.Store.GetApprovalRecord(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if resolved.Resolution != "allow_always" {
		t.Fatalf("expected allow_always resolution, got %q", resolved.Resolution)
	}
}

func TestTaskExpansionApprovalRecordsResolveCanonically(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.task-expand", "read", "write"))
	sc := newScenario(t, env, "task-expand")
	sc.activateService(t, env, "mock.task-expand")

	taskID := sc.createApprovedTask(t, env, "mock.task-expand", "read", true)

	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"service":      "mock.task-expand",
		"action":       "write",
		"auto_execute": false,
		"reason":       "write back the summary after review",
	})
	mustStatus(t, resp, http.StatusAccepted)

	rec := mustPendingTaskApprovalRecord(t, env.Store, sc.session.UserID, taskID, "task_expand")
	if rec.Status != "pending" {
		t.Fatalf("expected pending scope expansion record, got %q", rec.Status)
	}

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resolved, err := env.Store.GetApprovalRecord(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if resolved.Status != "approved" {
		t.Fatalf("expected approved status, got %q", resolved.Status)
	}
	if resolved.Resolution != "allow_session" {
		t.Fatalf("expected allow_session resolution, got %q", resolved.Resolution)
	}
}

func TestTaskDenyResolvesCanonicalApprovalRecord(t *testing.T) {
	env := newTestEnv(t, newMockAdapter("mock.task-deny", "read"))
	sc := newScenario(t, env, "task-deny")
	sc.activateService(t, env, "mock.task-deny")

	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "review account activity",
		"authorized_actions": []map[string]any{{
			"service": "mock.task-deny", "action": "read", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	rec := mustPendingTaskApprovalRecord(t, env.Store, sc.session.UserID, taskID, "task_create")

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/deny", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	resolved, err := env.Store.GetApprovalRecord(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if resolved.Status != "denied" {
		t.Fatalf("expected denied status, got %q", resolved.Status)
	}
	if resolved.Resolution != "deny" {
		t.Fatalf("expected deny resolution, got %q", resolved.Resolution)
	}
}

func mustPendingTaskApprovalRecord(t *testing.T, st store.Store, userID, taskID, kind string) *store.ApprovalRecord {
	t.Helper()
	recs, err := st.ListPendingApprovalRecords(context.Background(), userID)
	if err != nil {
		t.Fatalf("ListPendingApprovalRecords: %v", err)
	}
	for _, rec := range recs {
		if rec.Kind == kind && rec.TaskID != nil && *rec.TaskID == taskID {
			return rec
		}
	}
	t.Fatalf("pending approval record not found for task=%s kind=%s", taskID, kind)
	return nil
}
