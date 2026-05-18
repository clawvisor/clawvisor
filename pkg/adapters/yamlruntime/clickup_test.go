package yamlruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// loadClickupDef parses the bundled clickup.yaml from disk and points its
// base_url at the supplied mock server. Loading the real file (rather than
// a hand-built ServiceDef) means the test exercises the same definition
// that ships with the binary — schema regressions in the YAML are caught.
func loadClickupDef(t *testing.T, baseURL string) yamldef.ServiceDef {
	t.Helper()
	repoRoot := findClickupRepoRoot(t)
	path := filepath.Join(repoRoot, "internal/adapters/definitions/clickup.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading clickup.yaml: %v", err)
	}
	var def yamldef.ServiceDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		t.Fatalf("parsing clickup.yaml: %v", err)
	}
	def.API.BaseURL = baseURL
	return def
}

func findClickupRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := cwd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("could not locate repo root (no go.mod found while walking up)")
	return ""
}

// clickupMockCall captures one inbound HTTP request so subtests can assert
// against method/path/headers/body shape without re-implementing handler
// scaffolding for every action.
type clickupMockCall struct {
	method string
	path   string
	query  string
	auth   string
	body   map[string]any
}

func runClickup(t *testing.T, action string, params map[string]any, status int, respBody any) (*adapters.Result, *clickupMockCall) {
	t.Helper()
	call := &clickupMockCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call.method = r.Method
		call.path = r.URL.Path
		call.query = r.URL.RawQuery
		call.auth = r.Header.Get("Authorization")
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &call.body)
			}
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		w.Header().Set("Content-Type", "application/json")
		if respBody != nil {
			_ = json.NewEncoder(w).Encode(respBody)
		}
	}))
	t.Cleanup(srv.Close)
	adapter, err := New(loadClickupDef(t, srv.URL), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     action,
		Params:     params,
		Credential: testCred("pk_test"),
	})
	if err != nil {
		t.Fatalf("Execute %s: %v", action, err)
	}
	return result, call
}

func TestClickupAdapter_ListWorkspaces(t *testing.T) {
	result, call := runClickup(t, "list_workspaces", map[string]any{}, 0, map[string]any{
		"teams": []map[string]any{
			{"id": "9015363078", "name": "Workspace", "color": "#1b5e20"},
			{"id": "9015363079", "name": "Other", "color": "#ff0000"},
		},
	})
	if call.method != "GET" || call.path != "/team" {
		t.Errorf("unexpected request: %s %s", call.method, call.path)
	}
	// ClickUp personal tokens go in the Authorization header with no prefix —
	// regression test for that auth shape since most other adapters use Bearer.
	if call.auth != "pk_test" {
		t.Errorf("auth header: %q (expected raw token, no Bearer prefix)", call.auth)
	}
	if result.Summary != "2 workspace(s)" {
		t.Errorf("summary: %q", result.Summary)
	}
	items, ok := result.Data.([]map[string]any)
	if !ok || len(items) != 2 {
		t.Fatalf("data: %T %v", result.Data, result.Data)
	}
	if items[0]["id"] != "9015363078" {
		t.Errorf("first id: %v", items[0]["id"])
	}
}

func TestClickupAdapter_ListSpaces_PathInterpolation(t *testing.T) {
	_, call := runClickup(t, "list_spaces", map[string]any{"workspace_id": "9015363078"}, 0, map[string]any{
		"spaces": []map[string]any{
			{"id": "90151188681", "name": "Space", "private": false, "archived": false},
		},
	})
	if call.path != "/team/9015363078/space" {
		t.Errorf("path interpolation failed: %s", call.path)
	}
	if !strings.Contains(call.query, "archived=false") {
		t.Errorf("default archived=false should appear in query, got: %s", call.query)
	}
}

func TestClickupAdapter_GetTask_NestedFieldExtraction(t *testing.T) {
	// Verifies status.status path traversal and nullable priority handling —
	// both are easy to break with overzealous renaming.
	result, call := runClickup(t, "get_task", map[string]any{"task_id": "86bxf32em"}, 0, map[string]any{
		"id":          "86bxf32em",
		"name":        "Read book",
		"description": "",
		"status":      map[string]any{"status": "complete", "color": "#000"},
		"priority":    nil,
		"due_date":    "1707772080000",
		"url":         "https://app.clickup.com/t/86bxf32em",
	})
	if call.path != "/task/86bxf32em" {
		t.Errorf("path: %s", call.path)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type: %T", result.Data)
	}
	if data["status"] != "complete" {
		t.Errorf("status.status extraction failed: %v", data["status"])
	}
	// Nullable priority must not error out — the field is rendered as the
	// zero value (empty string) rather than dropped, which is fine.
	if _, has := data["priority"]; !has {
		t.Errorf("priority field should be present even when null in response")
	}
	if result.Summary != "Task 86bxf32em: Read book [complete]" {
		t.Errorf("summary: %q", result.Summary)
	}
}

func TestClickupAdapter_CreateTask_SparseBody(t *testing.T) {
	_, call := runClickup(t, "create_task",
		map[string]any{
			"list_id":     "901502741173",
			"name":        "Smoke",
			"description": "from test",
		},
		0,
		map[string]any{
			"id":   "86c9vj547",
			"name": "Smoke",
			"url":  "https://app.clickup.com/t/86c9vj547",
		},
	)
	if call.method != "POST" || call.path != "/list/901502741173/task" {
		t.Errorf("unexpected request: %s %s", call.method, call.path)
	}
	// body_mode: sparse — only the provided fields should be in the body.
	// Critical: optional fields like priority/assignees must not be sent as
	// zero values, since ClickUp's PUT/POST treat presence as "set this".
	if call.body["name"] != "Smoke" {
		t.Errorf("name field: %v", call.body["name"])
	}
	if call.body["description"] != "from test" {
		t.Errorf("description field: %v", call.body["description"])
	}
	if _, has := call.body["priority"]; has {
		t.Errorf("priority should not be in sparse body when unset, got: %v", call.body["priority"])
	}
	if _, has := call.body["assignees"]; has {
		t.Errorf("assignees should not be in sparse body when unset, got: %v", call.body["assignees"])
	}
}

func TestClickupAdapter_UpdateTask_Put(t *testing.T) {
	_, call := runClickup(t, "update_task",
		map[string]any{
			"task_id": "86c9vj547",
			"name":    "Updated",
		},
		0,
		map[string]any{"id": "86c9vj547", "name": "Updated", "url": "https://app.clickup.com/t/86c9vj547"},
	)
	if call.method != "PUT" {
		t.Errorf("update_task should use PUT, got %s", call.method)
	}
	if call.path != "/task/86c9vj547" {
		t.Errorf("path: %s", call.path)
	}
	if call.body["name"] != "Updated" {
		t.Errorf("body: %v", call.body)
	}
}

func TestClickupAdapter_DeleteTask_FixedSummary(t *testing.T) {
	// Regression for the "Deleted task <no value>" bug fixed in this PR —
	// the summary must not reference {{.task_id}} since DELETE returns null.
	result, call := runClickup(t, "delete_task", map[string]any{"task_id": "86c9vj547"}, 0, nil)
	if call.method != "DELETE" {
		t.Errorf("expected DELETE, got %s", call.method)
	}
	if result.Summary != "Task deleted" {
		t.Errorf("summary should be the fixed string 'Task deleted', got %q", result.Summary)
	}
}

func TestClickupAdapter_CreateTaskComment_UsesResponseID(t *testing.T) {
	// Summary references {{.id}} from the response (not request params),
	// since the comment-create endpoint returns the new comment's own id.
	result, _ := runClickup(t, "create_task_comment",
		map[string]any{"task_id": "86c9vj547", "comment_text": "hi"},
		0,
		map[string]any{
			"id":      float64(90150225280595),
			"hist_id": "5089074057300466416",
			"date":    float64(1779128591029),
		},
	)
	// The id is decoded as float64 from JSON, then templated — Go's text/template
	// prints floats without trailing zeros via %v, but the response field also
	// renders the same way. Either "Posted comment 9.0150225280595e+13" or the
	// integer form would be acceptable; we just need it not to be "<no value>".
	if result.Summary == "" || strings.Contains(result.Summary, "<no value>") {
		t.Errorf("summary should embed the response id, got %q", result.Summary)
	}
	if !strings.HasPrefix(result.Summary, "Posted comment ") {
		t.Errorf("summary prefix: %q", result.Summary)
	}
}

func TestClickupAdapter_ListFolderLists_NotExercisedLive(t *testing.T) {
	// Live smoke test couldn't hit this action because the test workspace
	// had no folders. This subtest locks in the path/method shape and the
	// "lists" data_path extraction so a future YAML edit can't silently
	// break it.
	result, call := runClickup(t, "list_folder_lists",
		map[string]any{"folder_id": "999"},
		0,
		map[string]any{
			"lists": []map[string]any{
				{"id": "1001", "name": "Sprint", "archived": false, "task_count": 7},
			},
		},
	)
	if call.path != "/folder/999/list" {
		t.Errorf("path: %s", call.path)
	}
	items := result.Data.([]map[string]any)
	if len(items) != 1 || items[0]["id"] != "1001" {
		t.Errorf("data: %v", result.Data)
	}
}

func TestClickupAdapter_FilterWorkspaceTasks_ArrayQuery(t *testing.T) {
	// Array query params (statuses, assignees, tags) must be repeated keys
	// or comma-joined depending on the runtime convention; assert one of
	// the array params at least appears in the query string so a runtime
	// regression on array encoding doesn't slip through.
	_, call := runClickup(t, "filter_workspace_tasks",
		map[string]any{
			"workspace_id": "9015363078",
			"statuses":     []string{"open", "in progress"},
			"include_closed": true,
		},
		0,
		map[string]any{"tasks": []map[string]any{}},
	)
	if call.path != "/team/9015363078/task" {
		t.Errorf("path: %s", call.path)
	}
	if !strings.Contains(call.query, "statuses=") {
		t.Errorf("array param 'statuses' not in query: %s", call.query)
	}
	if !strings.Contains(call.query, "include_closed=true") {
		t.Errorf("include_closed not in query: %s", call.query)
	}
}

func TestClickupAdapter_SetCustomFieldValue_DualPathParams(t *testing.T) {
	// Both task_id and field_id are path params — verify both interpolate.
	_, call := runClickup(t, "set_custom_field_value",
		map[string]any{
			"task_id":  "86c9vj547",
			"field_id": "deadbeef-1234",
			"value":    "high",
		},
		0,
		map[string]any{},
	)
	if call.method != "POST" {
		t.Errorf("method: %s", call.method)
	}
	if call.path != "/task/86c9vj547/field/deadbeef-1234" {
		t.Errorf("dual path interpolation failed: %s", call.path)
	}
	if call.body["value"] != "high" {
		t.Errorf("body: %v", call.body)
	}
}

func TestClickupAdapter_ListCustomFields(t *testing.T) {
	result, call := runClickup(t, "list_custom_fields",
		map[string]any{"list_id": "901502741173"},
		0,
		map[string]any{
			"fields": []map[string]any{
				{"id": "f1", "name": "Priority", "type": "drop_down", "hide_from_guests": false},
			},
		},
	)
	if call.path != "/list/901502741173/field" {
		t.Errorf("path: %s", call.path)
	}
	items := result.Data.([]map[string]any)
	if len(items) != 1 || items[0]["name"] != "Priority" {
		t.Errorf("data: %v", result.Data)
	}
}
