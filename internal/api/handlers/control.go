package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func LLMControlProtocolPayload() map[string]any {
	return map[string]any{
		"name":        "clawvisor-control",
		"description": "Use Clawvisor synthetic tools to ask the user for permission before attempting tool work that may be blocked.",
		"rules": []string{
			"Call cv_protocol to read this protocol.",
			"Call cv_task to create a task approval request.",
			"Do not curl localhost, loopback, clawvisor.local, or any /control URL for Clawvisor control.",
			"Creating a task requests permission. It does not grant permission until the user approves it.",
			"Prefer expected_tools_json for harness tools such as bash, exec_command, WebFetch, Read, Write, or Edit.",
		},
		"create_task": map[string]any{
			"tool": "cv_task",
			"body": map[string]any{
				"purpose": "Briefly explain the user-visible work you need permission to do.",
				"expected_tools_json": []map[string]any{{
					"tool_name":   "bash",
					"why":         "Describe the exact command pattern or operation you need, e.g. run curl to POST JSON to https://api.example.com/widgets.",
					"input_regex": "^curl\\s+-XPOST\\s+https://api\\.example\\.com/widgets\\b.*$",
				}},
				"intent_verification_mode": "strict",
				"expires_in_seconds":       600,
			},
		},
	}
}

type LLMControlExecutor struct {
	tasks *TasksHandler
}

func NewLLMControlExecutor(tasks *TasksHandler) *LLMControlExecutor {
	return &LLMControlExecutor{tasks: tasks}
}

func (e *LLMControlExecutor) ExecuteControl(ctx context.Context, req llmproxy.ControlExecutionRequest) (llmproxy.ControlExecutionResponse, error) {
	if e == nil {
		return llmproxy.ControlExecutionResponse{StatusCode: http.StatusServiceUnavailable, ErrorMessage: "control executor is not configured"}, nil
	}
	switch req.ToolName {
	case llmproxy.ControlToolProtocol:
		body, err := json.Marshal(LLMControlProtocolPayload())
		if err != nil {
			return llmproxy.ControlExecutionResponse{StatusCode: http.StatusInternalServerError, ErrorMessage: err.Error()}, nil
		}
		return llmproxy.ControlExecutionResponse{
			StatusCode:  http.StatusOK,
			ContentType: "application/json",
			Body:        body,
		}, nil
	case llmproxy.ControlToolTask:
		if e.tasks == nil {
			return llmproxy.ControlExecutionResponse{StatusCode: http.StatusServiceUnavailable, ErrorMessage: "task handler is not configured"}, nil
		}
		return e.invoke(ctx, req.Agent, http.MethodPost, "/api/tasks", req.Body, e.tasks.Create), nil
	default:
		return llmproxy.ControlExecutionResponse{
			StatusCode:   http.StatusNotFound,
			ContentType:  "application/json",
			Body:         []byte(`{"error":"unknown control tool","code":"NOT_FOUND"}`),
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
