package mcp

import (
	"encoding/json"
	"testing"
)

func TestToolDefsIncludeCanonicalTaskAliases(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range toolDefs() {
		seen[tool.Name] = true
	}
	for _, name := range []string{
		"create_task",
		"clawvisor_task_create",
		"clawvisor_task_start",
		"clawvisor_task_end",
	} {
		if !seen[name] {
			t.Fatalf("expected tool %q to be present", name)
		}
	}
}

func TestBuildInternalRequestCanonicalTaskAliases(t *testing.T) {
	t.Run("create alias", func(t *testing.T) {
		args := mustJSON(t, map[string]any{
			"purpose": "test",
			"authorized_actions": []map[string]any{{
				"service": "mock.echo",
				"action":  "echo",
			}},
			"wait": false,
		})
		route, body, err := buildInternalRequest("clawvisor_task_create", args)
		if err != nil {
			t.Fatalf("buildInternalRequest: %v", err)
		}
		if route.method != "POST" || route.pattern != "POST /api/tasks" || route.path != "/api/tasks" {
			t.Fatalf("unexpected route for create alias: %+v", route)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if _, ok := payload["wait"]; ok {
			t.Fatal("wait should be stripped from create alias body")
		}
	})

	t.Run("start alias", func(t *testing.T) {
		args := mustJSON(t, map[string]any{
			"task_id":            "task-123",
			"runtime_session_id": "session-abc",
		})
		route, body, err := buildInternalRequest("clawvisor_task_start", args)
		if err != nil {
			t.Fatalf("buildInternalRequest: %v", err)
		}
		if route.method != "POST" || route.pattern != "POST /api/tasks/{id}/start" {
			t.Fatalf("unexpected route for start alias: %+v", route)
		}
		if route.path != "/api/tasks/task-123/start?wait=true" {
			t.Fatalf("unexpected path for start alias: %q", route.path)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if payload["runtime_session_id"] != "session-abc" {
			t.Fatalf("unexpected body payload: %+v", payload)
		}
	})

	t.Run("end alias", func(t *testing.T) {
		args := mustJSON(t, map[string]any{"task_id": "task-123", "runtime_session_id": "session-abc"})
		route, body, err := buildInternalRequest("clawvisor_task_end", args)
		if err != nil {
			t.Fatalf("buildInternalRequest: %v", err)
		}
		if route.method != "POST" || route.pattern != "POST /api/tasks/{id}/end" {
			t.Fatalf("unexpected route for end alias: %+v", route)
		}
		if route.path != "/api/tasks/task-123/end" {
			t.Fatalf("unexpected path for end alias: %q", route.path)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if payload["runtime_session_id"] != "session-abc" {
			t.Fatalf("unexpected body payload: %+v", payload)
		}
	})
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return json.RawMessage(b)
}
