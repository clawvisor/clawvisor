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
		return internalRoute{"GET", "/api/skill/catalog", "GET /api/skill/catalog", nil}, nil, nil

	case "create_task":
		return internalRoute{"POST", "/api/tasks", "POST /api/tasks", nil}, arguments, nil

	case "get_task":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		path := "/api/tasks/" + id
		// Forward optional long-poll query params.
		var qp []string
		if w := getString("wait"); w == "true" {
			qp = append(qp, "wait=true")
		}
		if t := getString("timeout"); t != "" {
			qp = append(qp, "timeout="+t)
		}
		if len(qp) > 0 {
			path += "?" + strings.Join(qp, "&")
		}
		return internalRoute{"GET", path, "GET /api/tasks/{id}",
			map[string]string{"id": id}}, nil, nil

	case "complete_task":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		return internalRoute{"POST", "/api/tasks/" + id + "/complete", "POST /api/tasks/{id}/complete",
			map[string]string{"id": id}}, nil, nil

	case "expand_task":
		id := getString("task_id")
		if err := validatePathParam(id, "task_id"); err != nil {
			return internalRoute{}, nil, err
		}
		// Remove task_id from the body — the handler reads it from the path.
		body := make(map[string]json.RawMessage)
		for k, v := range args {
			if k != "task_id" {
				body[k] = v
			}
		}
		b, _ := json.Marshal(body)
		return internalRoute{"POST", "/api/tasks/" + id + "/expand", "POST /api/tasks/{id}/expand",
			map[string]string{"id": id}}, b, nil

	case "gateway_request":
		return internalRoute{"POST", "/api/gateway/request", "POST /api/gateway/request", nil}, arguments, nil

	default:
		return internalRoute{}, nil, fmt.Errorf("unknown tool: %s", toolName)
	}
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
