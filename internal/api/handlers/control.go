package handlers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type LLMControlHandler struct {
	BaseURL string
}

func NewLLMControlHandler(baseURL string) *LLMControlHandler {
	return &LLMControlHandler{BaseURL: baseURL}
}

func (h *LLMControlHandler) Capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"control_host": "https://clawvisor.local",
		"direct_url":   strings.TrimRight(h.BaseURL, "/") + "/control/skill",
		"base_path":    "/control",
		"note":         "clawvisor.local is synthetic and is handled inside proxy-lite tool calls. Use direct_url when fetching documentation from a shell.",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/control/skill", "purpose": "Return schemas and examples for Clawvisor control-plane calls."},
			{"method": "POST", "path": "/control/tasks", "purpose": "Create a task approval request for future tool use."},
			{"method": "GET", "path": "/control/tasks/{id}", "purpose": "Fetch task status."},
			{"method": "POST", "path": "/control/tasks/{id}/expand", "purpose": "Request additional scope for an existing task."},
		},
	})
}

func (h *LLMControlHandler) Skill(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control",
		"description": "Use this control plane to ask the user for permission before attempting tool work that may be blocked.",
		"base_url":    "https://clawvisor.local",
		"direct_docs": strings.TrimRight(h.BaseURL, "/") + "/control/skill",
		"rules": []string{
			"clawvisor.local is synthetic. Do not expect DNS lookup for the naked domain to work.",
			"Use direct_docs for reading these schemas from a shell.",
			"Use the synthetic https://clawvisor.local/control/tasks URL exactly when creating a task request through a proxied tool call; Clawvisor handles it inside proxy-lite before the shell runs.",
			"Creating or expanding a task requests permission. It does not grant permission until the user approves it.",
			"Prefer expected_tools_json for harness tools such as bash, exec_command, WebFetch, Read, Write, or Edit.",
		},
		"create_task": map[string]any{
			"method": "POST",
			"path":   "/control/tasks",
			"body": map[string]any{
				"purpose": "Briefly explain the user-visible work you need permission to do.",
				"expected_tools_json": []map[string]any{{
					"tool_name": "bash",
					"why":       "Describe the exact command pattern or operation you need, e.g. run curl to POST JSON to https://api.example.com/widgets.",
				}},
				"intent_verification_mode": "strict",
				"expires_in_seconds":       600,
			},
		},
		"expand_task": map[string]any{
			"method": "POST",
			"path":   "/control/tasks/{id}/expand",
			"body": map[string]any{
				"service":      "github",
				"action":       "create_issue",
				"auto_execute": true,
				"reason":       "Explain why the existing task scope is insufficient.",
			},
		},
	})
}

type LLMControlExecutor struct {
	control *LLMControlHandler
	tasks   *TasksHandler
}

func NewLLMControlExecutor(control *LLMControlHandler, tasks *TasksHandler) *LLMControlExecutor {
	return &LLMControlExecutor{control: control, tasks: tasks}
}

func (e *LLMControlExecutor) ExecuteControl(ctx context.Context, req llmproxy.ControlExecutionRequest) (llmproxy.ControlExecutionResponse, error) {
	if e == nil {
		return llmproxy.ControlExecutionResponse{StatusCode: http.StatusServiceUnavailable, ErrorMessage: "control executor is not configured"}, nil
	}
	pathOnly := req.Path
	if idx := strings.IndexByte(pathOnly, '?'); idx >= 0 {
		pathOnly = pathOnly[:idx]
	}
	switch {
	case req.Method == http.MethodGet && (pathOnly == "/control" || pathOnly == "/control/capabilities"):
		return e.invoke(ctx, req.Agent, http.MethodGet, req.Path, nil, e.control.Capabilities), nil
	case req.Method == http.MethodGet && pathOnly == "/control/skill":
		return e.invoke(ctx, req.Agent, http.MethodGet, req.Path, nil, e.control.Skill), nil
	case req.Method == http.MethodPost && pathOnly == "/control/tasks":
		return e.invoke(ctx, req.Agent, http.MethodPost, req.Path, req.Body, e.tasks.Create), nil
	case req.Method == http.MethodGet && strings.HasPrefix(pathOnly, "/control/tasks/"):
		taskID := strings.TrimPrefix(pathOnly, "/control/tasks/")
		return e.invokeTask(ctx, req.Agent, http.MethodGet, req.Path, nil, taskID, e.tasks.Get), nil
	case req.Method == http.MethodPost && strings.HasPrefix(pathOnly, "/control/tasks/") && strings.HasSuffix(pathOnly, "/expand"):
		taskID := strings.TrimSuffix(strings.TrimPrefix(pathOnly, "/control/tasks/"), "/expand")
		return e.invokeTask(ctx, req.Agent, http.MethodPost, req.Path, req.Body, taskID, e.tasks.Expand), nil
	default:
		return llmproxy.ControlExecutionResponse{
			StatusCode:   http.StatusNotFound,
			ContentType:  "application/json",
			Body:         []byte(`{"error":"unknown control endpoint","code":"NOT_FOUND"}`),
			ErrorMessage: "",
		}, nil
	}
}

func (e *LLMControlExecutor) invoke(ctx context.Context, agent *store.Agent, method, path string, body []byte, handler func(http.ResponseWriter, *http.Request)) llmproxy.ControlExecutionResponse {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req = req.WithContext(store.WithAgent(ctx, agent))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return controlResponseFromRecorder(rec)
}

func (e *LLMControlExecutor) invokeTask(ctx context.Context, agent *store.Agent, method, path string, body []byte, taskID string, handler func(http.ResponseWriter, *http.Request)) llmproxy.ControlExecutionResponse {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.SetPathValue("id", taskID)
	req = req.WithContext(store.WithAgent(ctx, agent))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return controlResponseFromRecorder(rec)
}

func controlResponseFromRecorder(rec *httptest.ResponseRecorder) llmproxy.ControlExecutionResponse {
	resp := rec.Result()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return llmproxy.ControlExecutionResponse{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}
}
