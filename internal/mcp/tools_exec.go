package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
)

// executeTool runs an MCP tool by building an internal HTTP request and calling
// the existing handler. The authenticated agent context from the original
// request is preserved, so all existing auth/restrictions/audit logic applies.
func executeTool(
	originalReq *http.Request,
	toolName string,
	arguments json.RawMessage,
	handlers map[string]http.Handler,
	logger *slog.Logger,
) (*ToolResult, error) {
	route, body, err := buildInternalRequest(toolName, arguments)
	if err != nil {
		return nil, err
	}

	handler, ok := handlers[route.pattern]
	if !ok {
		return nil, fmt.Errorf("no handler registered for %s", route.pattern)
	}

	// Build a synthetic HTTP request that carries the original context
	// (which includes the authenticated agent).
	internalReq, err := http.NewRequestWithContext(
		originalReq.Context(),
		route.method,
		route.path,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("building internal request: %w", err)
	}
	internalReq.Header.Set("Content-Type", "application/json")

	// Copy the original Authorization header so middleware can re-authenticate.
	if auth := originalReq.Header.Get("Authorization"); auth != "" {
		internalReq.Header.Set("Authorization", auth)
	}

	// Set path values so handlers can use r.PathValue() (populated by ServeMux
	// during normal routing, but we bypass the mux here).
	for k, v := range route.pathValues {
		internalReq.SetPathValue(k, v)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, internalReq)

	respBody := rec.Body.String()
	statusCode := rec.Code

	logger.Debug("mcp tool executed",
		"tool", toolName,
		"status", statusCode,
		"response_len", len(respBody),
	)

	contentType := rec.Header().Get("Content-Type")

	// For non-JSON responses (e.g. skill catalog returns text/markdown), return as-is.
	if !strings.Contains(contentType, "application/json") {
		return &ToolResult{
			Content: []ToolContent{{Type: "text", Text: respBody}},
		}, nil
	}

	// For JSON responses, check if it's an error.
	if statusCode >= 400 {
		return &ToolResult{
			Content: []ToolContent{{Type: "text", Text: respBody}},
			IsError: true,
		}, nil
	}

	return &ToolResult{
		Content: []ToolContent{{Type: "text", Text: respBody}},
	}, nil
}

// internalRoute describes the HTTP method, path, and mux pattern for an internal request.
type internalRoute struct {
	method     string
	path       string
	pattern    string            // the ServeMux pattern used to look up the handler
	pathValues map[string]string // path parameters for SetPathValue
}

// buildInternalRequest maps an MCP tool name + arguments to an HTTP route + body.
func buildInternalRequest(toolName string, arguments json.RawMessage) (internalRoute, []byte, error) {
	// Parse arguments into a generic map for extracting path params.
	var args map[string]json.RawMessage
	if len(arguments) > 0 && string(arguments) != "{}" {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return internalRoute{}, nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args == nil {
		args = make(map[string]json.RawMessage)
	}

	getString := func(key string) string {
		v, ok := args[key]
		if !ok {
			return ""
		}
		var s string
		json.Unmarshal(v, &s)
		return s
	}

	switch toolName {
	case "fetch_catalog":
		path := "/api/skill/catalog"
		if svc := getString("service"); svc != "" {
			path += "?service=" + svc
		}
		return internalRoute{"GET", path, "GET /api/skill/catalog", nil}, nil, nil

	case "create_task", "clawvisor_task_create":
		path := "/api/tasks"
		path += buildWaitQuery(args, getString, true)
		// Remove wait/timeout from body — they're query params, not task fields.
		body := stripKeys(args, "wait", "timeout")
		return internalRoute{"POST", path, "POST /api/tasks", nil}, body, nil

	case "get_task":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		path := "/api/tasks/" + id
		// get_task defaults wait to false (it's a status check, not a creation).
		path += buildWaitQuery(args, getString, false)
		return internalRoute{"GET", path, "GET /api/tasks/{id}",
			map[string]string{"id": id}}, nil, nil

	case "clawvisor_task_start":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		path := "/api/tasks/" + id + "/start"
		path += buildWaitQuery(args, getString, true)
		body := stripKeys(args, "task_id", "wait", "timeout")
		return internalRoute{"POST", path, "POST /api/tasks/{id}/start",
			map[string]string{"id": id}}, body, nil

	case "complete_task":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		return internalRoute{"POST", "/api/tasks/" + id + "/complete", "POST /api/tasks/{id}/complete",
			map[string]string{"id": id}}, nil, nil

	case "clawvisor_task_end":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		body := stripKeys(args, "task_id", "wait", "timeout")
		return internalRoute{"POST", "/api/tasks/" + id + "/end", "POST /api/tasks/{id}/end",
			map[string]string{"id": id}}, body, nil

	case "expand_task":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		path := "/api/tasks/" + id + "/expand"
		path += buildWaitQuery(args, getString, true)
		// Remove task_id, wait, timeout from the body — they're path/query params.
		body := stripKeys(args, "task_id", "wait", "timeout")
		return internalRoute{"POST", path, "POST /api/tasks/{id}/expand",
			map[string]string{"id": id}}, body, nil

	case "gateway_request":
		path := "/api/gateway/request"
		path += buildWaitQuery(args, getString, true)
		// Remove wait/timeout from body — they're query params, not request fields.
		body := stripKeys(args, "wait", "timeout")
		return internalRoute{"POST", path, "POST /api/gateway/request", nil}, body, nil

	case "gateway_batch":
		path := "/api/gateway/batch"
		path += buildWaitQuery(args, getString, true)
		body := stripKeys(args, "wait", "timeout")
		return internalRoute{"POST", path, "POST /api/gateway/batch", nil}, body, nil

	case "execute_request":
		id := getString("request_id")
		if err := validatePathParam(id, "request_id"); err != nil {
			return internalRoute{}, nil, err
		}
		path := "/api/gateway/request/" + id + "/execute"
		path += buildWaitQuery(args, getString, true)
		return internalRoute{"POST", path, "POST /api/gateway/request/{request_id}/execute",
			map[string]string{"request_id": id}}, nil, nil

	case "create_adapter":
		body, _ := json.Marshal(args)
		return internalRoute{"POST", "/api/adapters/generate", "POST /api/adapters/generate", nil}, body, nil

	case "update_adapter":
		id := getString("service_id")
		if err := validatePathParam(id, "service_id"); err != nil {
			return internalRoute{}, nil, err
		}
		body := stripKeys(args, "service_id")
		return internalRoute{"PUT", "/api/adapters/" + id + "/generate", "PUT /api/adapters/{service_id}/generate",
			map[string]string{"service_id": id}}, body, nil

	case "remove_adapter":
		id := getString("service_id")
		if err := validatePathParam(id, "service_id"); err != nil {
			return internalRoute{}, nil, err
		}
		return internalRoute{"DELETE", "/api/adapters/" + id, "DELETE /api/adapters/{service_id}",
			map[string]string{"service_id": id}}, nil, nil

	case "report_bug":
		body, _ := json.Marshal(args)
		return internalRoute{"POST", "/api/feedback/report", "POST /api/feedback/report", nil}, body, nil

	case "submit_nps":
		body, _ := json.Marshal(args)
		return internalRoute{"POST", "/api/feedback/nps", "POST /api/feedback/nps", nil}, body, nil

	default:
		return internalRoute{}, nil, fmt.Errorf("unknown tool: %s", toolName)
	}
}

// buildWaitQuery constructs the ?wait=true&timeout=N query string from MCP
// tool arguments. When defaultWait is true and the caller did not explicitly
// set wait=false, wait defaults to true (the natural fit for MCP tool calls).
func buildWaitQuery(args map[string]json.RawMessage, getString func(string) string, defaultWait bool) string {
	wait := defaultWait
	if w, ok := args["wait"]; ok {
		var b bool
		if json.Unmarshal(w, &b) == nil {
			wait = b
		}
	}
	if !wait {
		return ""
	}
	q := "?wait=true"
	if t := getString("timeout"); t != "" {
		q += "&timeout=" + t
	}
	return q
}

// stripKeys returns the JSON-encoded args map with the given keys removed.
func stripKeys(args map[string]json.RawMessage, keys ...string) []byte {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	body := make(map[string]json.RawMessage)
	for k, v := range args {
		if !drop[k] {
			body[k] = v
		}
	}
	b, _ := json.Marshal(body)
	return b
}

// validatePathParam checks that a path parameter is non-empty and contains
// no path separators or other metacharacters.
func validatePathParam(value, name string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(value, "/\\..") {
		return fmt.Errorf("%s contains invalid characters", name)
	}
	return nil
}
