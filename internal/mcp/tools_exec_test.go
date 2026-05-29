package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func args(t *testing.T, kv map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func TestBuildInternalRequest_FetchCatalog_ServiceParam(t *testing.T) {
	tests := []struct {
		name        string
		service     any
		wantPath    string
		wantNoParam string // must NOT appear as a separate query param
	}{
		{
			name:     "normal service value",
			service:  "google.gmail",
			wantPath: "/api/skill/catalog?service=google.gmail",
		},
		{
			name:        "injection via ampersand",
			service:     "google.gmail&admin=true",
			wantPath:    "/api/skill/catalog?service=google.gmail%26admin%3Dtrue",
			wantNoParam: "&admin=true",
		},
		{
			name:        "injection via equals",
			service:     "google.gmail=foo",
			wantPath:    "/api/skill/catalog?service=google.gmail%3Dfoo",
			wantNoParam: "&foo",
		},
		{
			name:     "spaces encoded",
			service:  "google gmail",
			wantPath: "/api/skill/catalog?service=google+gmail",
		},
		{
			name:     "empty service omits query string",
			service:  "",
			wantPath: "/api/skill/catalog",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a json.RawMessage
			if tt.service == "" {
				a = args(t, map[string]any{})
			} else {
				a = args(t, map[string]any{"service": tt.service})
			}
			route, _, err := buildInternalRequest("fetch_catalog", a)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if route.path != tt.wantPath {
				t.Errorf("path = %q, want %q", route.path, tt.wantPath)
			}
			if tt.wantNoParam != "" && strings.Contains(route.path, tt.wantNoParam) {
				t.Errorf("path %q contains injected param %q", route.path, tt.wantNoParam)
			}
		})
	}
}

func TestBuildInternalRequest_TimeoutParam(t *testing.T) {
	tests := []struct {
		name        string
		toolName    string
		extraArgs   map[string]any
		wantContains string
		wantMissing  string
	}{
		{
			name:         "normal timeout value",
			toolName:     "gateway_request",
			extraArgs:    map[string]any{"wait": true, "timeout": "120"},
			wantContains: "timeout=120",
		},
		{
			name:         "integer timeout value",
			toolName:     "gateway_request",
			extraArgs:    map[string]any{"wait": true, "timeout": 60},
			wantContains: "timeout=60",
		},
		{
			name:         "integer timeout get_task",
			toolName:     "get_task",
			extraArgs:    map[string]any{"task_id": "123", "wait": true, "timeout": 45},
			wantContains: "timeout=45",
		},
		{
			name:        "injection via ampersand in timeout",
			toolName:    "gateway_request",
			extraArgs:   map[string]any{"wait": true, "timeout": "120&admin=true"},
			wantContains: "timeout=120%26admin%3Dtrue",
			wantMissing:  "&admin=true",
		},
		{
			name:        "injection via equals in timeout",
			toolName:    "gateway_request",
			extraArgs:   map[string]any{"wait": true, "timeout": "120=foo"},
			wantContains: "timeout=120%3Dfoo",
			wantMissing:  "=foo&",
		},
		{
			name:         "no timeout omits timeout param",
			toolName:     "gateway_request",
			extraArgs:    map[string]any{"wait": true},
			wantContains: "?wait=true",
			wantMissing:  "timeout",
		},
		{
			name:        "wait=false ignores timeout entirely",
			toolName:    "gateway_request",
			extraArgs:   map[string]any{"wait": false, "timeout": "120&admin=true"},
			wantMissing: "wait",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := args(t, tt.extraArgs)
			route, _, err := buildInternalRequest(tt.toolName, a)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantContains != "" && !strings.Contains(route.path, tt.wantContains) {
				t.Errorf("path %q does not contain %q", route.path, tt.wantContains)
			}
			if tt.wantMissing != "" && strings.Contains(route.path, tt.wantMissing) {
				t.Errorf("path %q should not contain %q", route.path, tt.wantMissing)
			}
		})
	}
}
