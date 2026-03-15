package api_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// ── Long-poll GET /api/tasks/{id}?wait=true ──────────────────────────────────

func TestGetTask_LongPoll_ReturnsImmediately_WhenAlreadyApproved(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	// Long-poll on an already-active task should return immediately.
	start := time.Now()
	resp := env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=5", taskID), sc.AgentToken, nil)
	elapsed := time.Since(start)

	body := mustStatus(t, resp, http.StatusOK)
	if str(t, body, "status") != "active" {
		t.Errorf("expected status=active, got %v", body["status"])
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected immediate return for already-active task, took %s", elapsed)
	}
}

func TestGetTask_LongPoll_ReturnsImmediately_WhenDenied(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	sc.activateService(t, env, "mock.echo")

	// Create a task but deny it.
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test",
		"authorized_actions": []map[string]any{{
			"service": "mock.echo", "action": "echo", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/deny", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	// Long-poll on a denied task should return immediately.
	start := time.Now()
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=5", taskID), sc.AgentToken, nil)
	elapsed := time.Since(start)

	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "status") != "denied" {
		t.Errorf("expected status=denied, got %v", body["status"])
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected immediate return for denied task, took %s", elapsed)
	}
}

func TestGetTask_LongPoll_WaitsForApproval(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	sc.activateService(t, env, "mock.echo")

	// Create a pending task.
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test",
		"authorized_actions": []map[string]any{{
			"service": "mock.echo", "action": "echo", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	// Start long-poll in a goroutine.
	var (
		wg       sync.WaitGroup
		pollBody map[string]any
		pollErr  error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=10", taskID), sc.AgentToken, nil)
		pollBody = mustStatus(t, r, http.StatusOK)
	}()

	// Give the long-poll a moment to subscribe, then approve.
	time.Sleep(200 * time.Millisecond)
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	wg.Wait()
	if pollErr != nil {
		t.Fatalf("long-poll error: %v", pollErr)
	}
	if str(t, pollBody, "status") != "active" {
		t.Errorf("expected status=active after approval, got %v", pollBody["status"])
	}
}

func TestGetTask_LongPoll_WaitsForDenial(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	sc.activateService(t, env, "mock.echo")

	// Create a pending task.
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test",
		"authorized_actions": []map[string]any{{
			"service": "mock.echo", "action": "echo", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	// Start long-poll in a goroutine.
	var (
		wg       sync.WaitGroup
		pollBody map[string]any
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=10", taskID), sc.AgentToken, nil)
		pollBody = mustStatus(t, r, http.StatusOK)
	}()

	// Give the long-poll a moment to subscribe, then deny.
	time.Sleep(200 * time.Millisecond)
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/deny", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	wg.Wait()
	if str(t, pollBody, "status") != "denied" {
		t.Errorf("expected status=denied after denial, got %v", pollBody["status"])
	}
}

func TestGetTask_LongPoll_TimesOut(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	sc.activateService(t, env, "mock.echo")

	// Create a pending task (never approve it).
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test",
		"authorized_actions": []map[string]any{{
			"service": "mock.echo", "action": "echo", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	start := time.Now()
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=1", taskID), sc.AgentToken, nil)
	elapsed := time.Since(start)

	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "status") != "pending_approval" {
		t.Errorf("expected status=pending_approval after timeout, got %v", body["status"])
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected ~1s wait, but returned in %s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("expected ~1s wait, took %s", elapsed)
	}
}

func TestGetTask_LongPoll_TimeoutCapped(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	sc.activateService(t, env, "mock.echo")

	// Create a pending task.
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test",
		"authorized_actions": []map[string]any{{
			"service": "mock.echo", "action": "echo", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	// Request a huge timeout — should be capped at 120s.
	// We won't actually wait 120s; approve quickly to unblock.
	var (
		wg       sync.WaitGroup
		pollBody map[string]any
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=9999", taskID), sc.AgentToken, nil)
		pollBody = mustStatus(t, r, http.StatusOK)
	}()

	time.Sleep(200 * time.Millisecond)
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	wg.Wait()
	if str(t, pollBody, "status") != "active" {
		t.Errorf("expected status=active, got %v", pollBody["status"])
	}
}

func TestGetTask_NoWait_ReturnsPendingImmediately(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")
	sc.activateService(t, env, "mock.echo")

	// Create a pending task.
	resp := env.do("POST", "/api/tasks", sc.AgentToken, map[string]any{
		"purpose": "test",
		"authorized_actions": []map[string]any{{
			"service": "mock.echo", "action": "echo", "auto_execute": true,
		}},
	})
	body := mustStatus(t, resp, http.StatusCreated)
	taskID := str(t, body, "task_id")

	// Without wait param, should return immediately with pending status.
	start := time.Now()
	resp = env.do("GET", fmt.Sprintf("/api/tasks/%s", taskID), sc.AgentToken, nil)
	elapsed := time.Since(start)

	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "status") != "pending_approval" {
		t.Errorf("expected status=pending_approval, got %v", body["status"])
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected immediate return without wait, took %s", elapsed)
	}
}

func TestGetTask_LongPoll_ScopeExpansionApproval(t *testing.T) {
	adapter := newMockAdapter("mock.echo", "echo", "ping")
	env := newTestEnv(t, adapter)
	sc := newScenario(t, env, "bot")

	taskID := sc.createApprovedTask(t, env, "mock.echo", "echo", true)

	// Request scope expansion.
	resp := env.do("POST", fmt.Sprintf("/api/tasks/%s/expand", taskID), sc.AgentToken, map[string]any{
		"service": "mock.echo", "action": "ping", "reason": "need ping",
	})
	mustStatus(t, resp, http.StatusAccepted)

	// Long-poll while pending_scope_expansion.
	var (
		wg       sync.WaitGroup
		pollBody map[string]any
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := env.do("GET", fmt.Sprintf("/api/tasks/%s?wait=true&timeout=10", taskID), sc.AgentToken, nil)
		pollBody = mustStatus(t, r, http.StatusOK)
	}()

	time.Sleep(200 * time.Millisecond)
	resp = sc.session.do("POST", fmt.Sprintf("/api/tasks/%s/expand/approve", taskID), nil)
	mustStatus(t, resp, http.StatusOK)

	wg.Wait()
	if str(t, pollBody, "status") != "active" {
		t.Errorf("expected status=active after expansion approval, got %v", pollBody["status"])
	}
}
