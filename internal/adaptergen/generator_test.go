package adaptergen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// sampleSlackMCPTools is a minimal MCP tool definition for testing.
const sampleSlackMCPTools = `[
  {
    "name": "list_channels",
    "description": "List all Slack channels",
    "inputSchema": {
      "type": "object",
      "properties": {
        "limit": {"type": "integer", "description": "Max results"}
      }
    }
  },
  {
    "name": "send_message",
    "description": "Send a message to a Slack channel",
    "inputSchema": {
      "type": "object",
      "properties": {
        "channel": {"type": "string", "description": "Channel ID"},
        "text": {"type": "string", "description": "Message text"}
      },
      "required": ["channel", "text"]
    }
  }
]`

// oaResponse builds an OpenAI-compatible chat completions response.
func oaResponse(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"content": content}},
		},
	})
	return b
}

// newMockLLMServer creates a test HTTP server that responds to LLM requests.
// The handler receives each request and can return different responses based on
// the system prompt (generation vs risk classification).
func newMockLLMServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, config.AdapterGenConfig) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, config.AdapterGenConfig{
		LLMProviderConfig: config.LLMProviderConfig{
			Enabled:        true,
			Endpoint:       ts.URL + "/v1",
			APIKey:         "test-key",
			Model:          "test-model",
			TimeoutSeconds: 10,
		},
	}
}

// generatedYAML is a valid adapter YAML that the mock LLM returns for generation.
const generatedYAML = `service:
  id: testapi
  display_name: Test API
  description: A test API service
auth:
  type: api_key
  header: Authorization
  header_prefix: "Bearer "
api:
  base_url: https://api.test.com
  type: rest
actions:
  list_items:
    display_name: List items
    risk:
      category: UNCLASSIFIED
      sensitivity: UNCLASSIFIED
      description: List all items
    method: GET
    path: /items
    params:
      limit:
        type: int
        location: query
    response:
      fields:
        - { name: id }
        - { name: name }
      summary: "{{len .Data}} items"
  create_item:
    display_name: Create item
    risk:
      category: UNCLASSIFIED
      sensitivity: UNCLASSIFIED
      description: Create a new item
    method: POST
    path: /items
    params:
      name:
        type: string
        required: true
        location: body
    response:
      fields:
        - { name: id }
      summary: "Item created"
`

// riskJSON is the risk classification response the mock LLM returns.
const riskJSON = `{"list_items":{"category":"read","sensitivity":"low"},"create_item":{"category":"write","sensitivity":"medium"}}`

func TestGenerate_EndToEnd(t *testing.T) {
	callCount := 0
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// First call: generation
			w.Write(oaResponse(generatedYAML))
		} else {
			// Second call: risk classification
			w.Write(oaResponse(riskJSON))
		}
	})

	tmpDir := t.TempDir()
	adaptersDir := filepath.Join(tmpDir, "adapters")
	registry := adapters.NewRegistry()

	gen := New(cfg, registry, adaptersDir, nil)

	result, err := gen.Generate(context.Background(), Source{
		Type:    SourceMCP,
		Content: sampleSlackMCPTools,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if result.ServiceID != "testapi" {
		t.Errorf("ServiceID: got %q, want testapi", result.ServiceID)
	}
	if result.Installed {
		t.Error("expected adapter to NOT be installed after Generate (preview only)")
	}
	if len(result.Actions) != 2 {
		t.Errorf("Actions: got %d, want 2", len(result.Actions))
	}

	// Verify risk was classified (not UNCLASSIFIED).
	if strings.Contains(result.YAML, "UNCLASSIFIED") {
		t.Error("generated YAML still contains UNCLASSIFIED risk")
	}

	// Now install explicitly.
	installed, err := gen.Install(result.YAML)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !installed.Installed {
		t.Error("expected adapter to be installed after Install")
	}

	// Verify adapter is in registry.
	a, ok := registry.Get("testapi")
	if !ok {
		t.Fatal("adapter not found in registry after install")
	}
	if len(a.SupportedActions()) != 2 {
		t.Errorf("registry adapter actions: got %d, want 2", len(a.SupportedActions()))
	}

	// Verify YAML file was written to disk.
	yamlPath := filepath.Join(adaptersDir, "testapi.yaml")
	if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
		t.Error("adapter YAML file not written to disk")
	}
}

func TestGenerate_TwoLLMCalls(t *testing.T) {
	callCount := 0
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			w.Write(oaResponse(generatedYAML))
		} else {
			w.Write(oaResponse(riskJSON))
		}
	})

	gen := New(cfg, adapters.NewRegistry(), t.TempDir(), nil)
	_, err := gen.Generate(context.Background(), Source{
		Type:    SourceMCP,
		Content: sampleSlackMCPTools,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 LLM calls (generation + risk), got %d", callCount)
	}
}

func TestGenerate_StripsMarkdownFences(t *testing.T) {
	callCount := 0
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// LLM wraps YAML in code fences despite instructions.
			w.Write(oaResponse("```yaml\n" + generatedYAML + "\n```"))
		} else {
			w.Write(oaResponse("```json\n" + riskJSON + "\n```"))
		}
	})

	gen := New(cfg, adapters.NewRegistry(), t.TempDir(), nil)
	result, err := gen.Generate(context.Background(), Source{
		Type:    SourceMCP,
		Content: sampleSlackMCPTools,
	})
	if err != nil {
		t.Fatalf("Generate with fences: %v", err)
	}
	if result.Installed {
		t.Error("expected adapter to NOT be installed (preview only)")
	}
	if result.ServiceID == "" {
		t.Error("expected non-empty service ID even with code fences")
	}
}

func TestGenerate_EmptySource(t *testing.T) {
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("LLM should not be called with empty source")
	})

	gen := New(cfg, adapters.NewRegistry(), t.TempDir(), nil)
	_, err := gen.Generate(context.Background(), Source{
		Type:    SourceMCP,
		Content: "",
	})
	if err == nil {
		t.Fatal("expected error for empty source")
	}
}

func TestGenerate_InvalidSourceType(t *testing.T) {
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("LLM should not be called with invalid source type")
	})

	gen := New(cfg, adapters.NewRegistry(), t.TempDir(), nil)
	_, err := gen.Generate(context.Background(), Source{
		Type:    "invalid",
		Content: "some content",
	})
	if err == nil {
		t.Fatal("expected error for invalid source type")
	}
}

func TestRemove(t *testing.T) {
	// Set up: generate an adapter first.
	callCount := 0
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			w.Write(oaResponse(generatedYAML))
		} else {
			w.Write(oaResponse(riskJSON))
		}
	})

	tmpDir := t.TempDir()
	adaptersDir := filepath.Join(tmpDir, "adapters")
	registry := adapters.NewRegistry()
	gen := New(cfg, registry, adaptersDir, nil)

	result, err := gen.Generate(context.Background(), Source{
		Type:    SourceMCP,
		Content: sampleSlackMCPTools,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Install so the file is written to disk and adapter is in the registry.
	_, err = gen.Install(result.YAML)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Verify file exists before removal.
	yamlPath := filepath.Join(adaptersDir, "testapi.yaml")
	if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
		t.Fatal("adapter file should exist before removal")
	}

	// Remove it.
	if err := gen.Remove("testapi"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Verify file is gone.
	if _, err := os.Stat(yamlPath); !os.IsNotExist(err) {
		t.Error("adapter file should be removed from disk")
	}
}

func TestRemove_NotFound(t *testing.T) {
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {})
	gen := New(cfg, adapters.NewRegistry(), t.TempDir(), nil)

	err := gen.Remove("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent adapter")
	}
}

func TestUpdate_NotFound(t *testing.T) {
	_, cfg := newMockLLMServer(t, func(w http.ResponseWriter, r *http.Request) {})
	gen := New(cfg, adapters.NewRegistry(), t.TempDir(), nil)

	_, err := gen.Update(context.Background(), "nonexistent", Source{
		Type:    SourceMCP,
		Content: sampleSlackMCPTools,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent adapter")
	}
}

// ── Path traversal tests ───────────────────────────────────────────────────────

func TestSafeFilename_Valid(t *testing.T) {
	dir := t.TempDir()
	path, err := safeFilename(dir, "google.gmail")
	if err != nil {
		t.Fatalf("safeFilename: %v", err)
	}
	if !strings.HasSuffix(path, "google_gmail.yaml") {
		t.Errorf("expected google_gmail.yaml suffix, got %s", path)
	}
}

func TestSafeFilename_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	cases := []string{
		"../../tmp/evil",
		"../evil",
		"/etc/evil",
		"foo/../../bar",
	}
	for _, id := range cases {
		_, err := safeFilename(dir, id)
		if err == nil {
			t.Errorf("safeFilename(%q): expected error for path traversal", id)
		}
	}
}

// ── Validator tests ─────────────────────────────────────────────────────────────

func TestValidate_ServiceIDFormat(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"slack", true},
		{"google.gmail", true},
		{"my.cool.service", true},
		{"UPPER", false},
		{"has spaces", false},
		{"../../etc", false},
		{"has/slash", false},
		{"", false},
		{".leading.dot", false},
	}
	for _, tc := range cases {
		got := safeServiceIDPattern.MatchString(tc.id)
		if got != tc.valid {
			t.Errorf("safeServiceIDPattern(%q): got %v, want %v", tc.id, got, tc.valid)
		}
	}
}

func TestValidate_RiskSanity(t *testing.T) {
	// DELETE method with "read" category should fail validation.
	yaml := `service:
  id: test
  display_name: Test
  description: Test
auth:
  type: api_key
api:
  base_url: https://api.test.com
  type: rest
actions:
  delete_item:
    display_name: Delete item
    risk:
      category: read
      sensitivity: low
      description: Delete an item
    method: DELETE
    path: /items/{id}
`
	_, errs, warnings, err := parseAndValidate([]byte(yaml))
	if err != nil {
		t.Fatalf("parseAndValidate: %v", err)
	}
	// The bad action should be dropped (warning), leaving zero actions (error).
	if len(errs) == 0 {
		t.Fatal("expected validation errors after dropping bad action")
	}
	if len(warnings) == 0 {
		t.Fatal("expected warnings about dropped action")
	}

	foundCategory := false
	foundSensitivity := false
	for _, w := range warnings {
		if strings.Contains(w, "risk category 'delete'") {
			foundCategory = true
		}
		if strings.Contains(w, "cannot have 'low' sensitivity") {
			foundSensitivity = true
		}
	}
	if !foundCategory {
		t.Error("expected warning about DELETE needing 'delete' category")
	}
	if !foundSensitivity {
		t.Error("expected warning about DELETE not allowing 'low' sensitivity")
	}
}

func TestStripCodeFences(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"```yaml\nfoo: bar\n```", "foo: bar"},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"no fences", "no fences"},
		{"```\nplain\n```", "plain"},
	}
	for _, tc := range cases {
		got := stripCodeFences(tc.input)
		if got != tc.want {
			t.Errorf("stripCodeFences(%q): got %q, want %q", tc.input, got, tc.want)
		}
	}
}
