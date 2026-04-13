package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// ── diagnoseJSONError ────────────────────────────────────────────────────────

func TestDiagnoseJSONError_EmptyBody(t *testing.T) {
	d := diagnoseJSONError(io.EOF)
	if d.Code != "INVALID_REQUEST" {
		t.Fatalf("expected code INVALID_REQUEST, got %s", d.Code)
	}
	if !strings.Contains(d.Hint, "empty") && !strings.Contains(d.Error, "empty") {
		t.Fatalf("expected mention of empty body, got error=%q hint=%q", d.Error, d.Hint)
	}
}

func TestDiagnoseJSONError_TruncatedBody(t *testing.T) {
	d := diagnoseJSONError(io.ErrUnexpectedEOF)
	if !strings.Contains(d.Error, "incomplete") {
		t.Fatalf("expected mention of incomplete JSON, got %q", d.Error)
	}
	if d.Hint == "" {
		t.Fatal("expected a hint")
	}
}

func TestDiagnoseJSONError_SyntaxError(t *testing.T) {
	// Trigger a real syntax error by decoding invalid JSON.
	var v any
	err := json.Unmarshal([]byte(`{"key": ,}`), &v)
	if err == nil {
		t.Fatal("expected error")
	}
	d := diagnoseJSONError(err)
	if d.Code != "INVALID_REQUEST" {
		t.Fatalf("expected code INVALID_REQUEST, got %s", d.Code)
	}
	if d.Hint == "" {
		t.Fatal("expected a hint for syntax errors")
	}
}

func TestDiagnoseJSONError_TypeMismatch(t *testing.T) {
	var v struct {
		Count int `json:"count"`
	}
	err := json.Unmarshal([]byte(`{"count": "not_a_number"}`), &v)
	if err == nil {
		t.Fatal("expected error")
	}
	d := diagnoseJSONError(err)
	if !strings.Contains(d.Error, "count") {
		t.Fatalf("expected field name in error, got %q", d.Error)
	}
	if !strings.Contains(d.Hint, "type") {
		t.Fatalf("expected type info in hint, got %q", d.Hint)
	}
}

func TestDiagnoseJSONError_StringInsteadOfObject(t *testing.T) {
	var v struct {
		Name string `json:"name"`
	}
	err := json.Unmarshal([]byte(`"just a string"`), &v)
	if err == nil {
		t.Fatal("expected error")
	}
	d := diagnoseJSONError(err)
	if !strings.Contains(d.Error, "expected a JSON object") {
		t.Fatalf("expected top-level type mismatch error, got %q", d.Error)
	}
	if !strings.Contains(d.Error, "string") {
		t.Fatalf("expected error to mention 'string', got %q", d.Error)
	}
	if !strings.Contains(d.Hint, "object") {
		t.Fatalf("expected hint to mention object, got %q", d.Hint)
	}
}

func TestDiagnoseJSONError_ArrayInsteadOfObject(t *testing.T) {
	var v struct {
		Name string `json:"name"`
	}
	err := json.Unmarshal([]byte(`[1, 2, 3]`), &v)
	if err == nil {
		t.Fatal("expected error")
	}
	d := diagnoseJSONError(err)
	if !strings.Contains(d.Error, "expected a JSON object") {
		t.Fatalf("expected top-level type mismatch error, got %q", d.Error)
	}
	if !strings.Contains(d.Error, "array") {
		t.Fatalf("expected error to mention 'array', got %q", d.Error)
	}
}

func TestFriendlyTypeName(t *testing.T) {
	cases := []struct {
		goType string
		want   string
	}{
		{"string", "a string"},
		{"int", "a number"},
		{"float64", "a number"},
		{"bool", "a boolean"},
		{"map[string]interface {}", "an object"},
		{"map[string]any", "an object"},
		{"[]string", "an array"},
		{"[]interface {}", "an array"},
		{"store.TaskAction", "an object (TaskAction)"},
		{"gateway.Request", "an object (Request)"},
	}
	for _, c := range cases {
		got := friendlyTypeName(c.goType)
		if got != c.want {
			t.Errorf("friendlyTypeName(%q) = %q, want %q", c.goType, got, c.want)
		}
	}
}

func TestDiagnoseJSONError_TypeMismatch_FriendlyHint(t *testing.T) {
	// Verify the hint doesn't contain Go type syntax like "map[string]interface {}"
	var v struct {
		Count int `json:"count"`
	}
	err := json.Unmarshal([]byte(`{"count": "not_a_number"}`), &v)
	d := diagnoseJSONError(err)
	if strings.Contains(d.Hint, "map[") || strings.Contains(d.Hint, "interface") {
		t.Fatalf("hint contains Go type syntax: %q", d.Hint)
	}
}

// ── decodeJSON integration ──────────────────────────────────────────────────

func TestDecodeJSON_ValidBody(t *testing.T) {
	body := strings.NewReader(`{"name": "test"}`)
	r := httptest.NewRequest(http.MethodPost, "/", body)
	w := httptest.NewRecorder()

	var v struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &v) {
		t.Fatal("expected decodeJSON to succeed")
	}
	if v.Name != "test" {
		t.Fatalf("expected name=test, got %q", v.Name)
	}
}

func TestDecodeJSON_EmptyBody_ReturnsDetailedError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	w := httptest.NewRecorder()

	var v any
	if decodeJSON(w, r, &v) {
		t.Fatal("expected decodeJSON to fail on empty body")
	}

	var resp apiErrorDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Code != "INVALID_REQUEST" {
		t.Fatalf("expected code INVALID_REQUEST, got %s", resp.Code)
	}
	if resp.Hint == "" {
		t.Fatal("expected a hint in the error response")
	}
}

func TestDecodeJSON_MalformedJSON_ReturnsHint(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"key": true,}`))
	w := httptest.NewRecorder()

	var v any
	if decodeJSON(w, r, &v) {
		t.Fatal("expected decodeJSON to fail on malformed JSON")
	}

	var resp apiErrorDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Hint == "" {
		t.Fatal("expected a hint for malformed JSON")
	}
}

// ── writeDetailedError ──────────────────────────────────────────────────────

func TestWriteDetailedError_IncludesAllFields(t *testing.T) {
	w := httptest.NewRecorder()
	writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
		Error:         "test error",
		Code:          "TEST_CODE",
		Hint:          "try this instead",
		MissingFields: []string{"field_a", "field_b"},
		Example:       map[string]any{"field_a": "value", "field_b": 42},
		Available:     []string{"opt1", "opt2"},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp["error"] != "test error" {
		t.Fatalf("wrong error: %v", resp["error"])
	}
	if resp["code"] != "TEST_CODE" {
		t.Fatalf("wrong code: %v", resp["code"])
	}
	if resp["hint"] != "try this instead" {
		t.Fatalf("wrong hint: %v", resp["hint"])
	}
	if resp["missing_fields"] == nil {
		t.Fatal("expected missing_fields")
	}
	if resp["example"] == nil {
		t.Fatal("expected example")
	}
	if resp["available"] == nil {
		t.Fatal("expected available")
	}
}

func TestWriteDetailedError_OmitsEmptyFields(t *testing.T) {
	w := httptest.NewRecorder()
	writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
		Error: "minimal error",
		Code:  "MIN",
	})

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if _, ok := resp["hint"]; ok {
		t.Fatal("hint should be omitted when empty")
	}
	if _, ok := resp["missing_fields"]; ok {
		t.Fatal("missing_fields should be omitted when empty")
	}
	if _, ok := resp["example"]; ok {
		t.Fatal("example should be omitted when empty")
	}
	if _, ok := resp["available"]; ok {
		t.Fatal("available should be omitted when empty")
	}
}

// ── validateRequestParams ───────────────────────────────────────────────────

type mockParamAdapter struct {
	adapters.Adapter // embed to satisfy interface
	params           map[string][]adapters.ParamInfo
}

func (m *mockParamAdapter) ActionParams(action string) []adapters.ParamInfo {
	return m.params[action]
}

func TestValidateRequestParams_AllPresent(t *testing.T) {
	adapter := &mockParamAdapter{
		params: map[string][]adapters.ParamInfo{
			"send": {
				{Name: "to", Type: "string", Required: true},
				{Name: "body", Type: "string", Required: true},
			},
		},
	}
	result, warns := validateRequestParams(adapter, "send", map[string]any{
		"to":   "alice",
		"body": "hello",
	})
	if result != nil {
		t.Fatalf("expected nil error, got %+v", result)
	}
	if len(warns) != 0 {
		t.Fatalf("expected no warnings, got %v", warns)
	}
}

func TestValidateRequestParams_MissingRequired(t *testing.T) {
	adapter := &mockParamAdapter{
		params: map[string][]adapters.ParamInfo{
			"send": {
				{Name: "to", Type: "string", Required: true},
				{Name: "body", Type: "string", Required: true},
				{Name: "subject", Type: "string", Required: false},
			},
		},
	}
	result, _ := validateRequestParams(adapter, "send", map[string]any{
		"subject": "hi",
	})
	if result == nil {
		t.Fatal("expected error detail for missing params")
	}
	if result.Code != "INVALID_PARAMS" {
		t.Fatalf("expected code INVALID_PARAMS, got %s", result.Code)
	}
	if len(result.MissingFields) != 2 {
		t.Fatalf("expected 2 missing fields, got %d: %v", len(result.MissingFields), result.MissingFields)
	}
	if result.Example == nil {
		t.Fatal("expected example in error")
	}
}

func TestValidateRequestParams_NoDescriber(t *testing.T) {
	// An adapter that doesn't implement ActionParamDescriber should pass.
	result, warns := validateRequestParams(nil, "send", nil)
	if result != nil {
		t.Fatalf("expected nil for nil adapter, got %+v", result)
	}
	if len(warns) != 0 {
		t.Fatalf("expected no warnings, got %v", warns)
	}
}

func TestValidateRequestParams_UnknownParams_Warning(t *testing.T) {
	adapter := &mockParamAdapter{
		params: map[string][]adapters.ParamInfo{
			"send": {
				{Name: "to", Type: "string", Required: true},
				{Name: "body", Type: "string", Required: true},
				{Name: "subject", Type: "string", Required: false},
			},
		},
	}
	result, warns := validateRequestParams(adapter, "send", map[string]any{
		"to":       "alice",
		"body":     "hello",
		"subjectt": "typo",  // close to "subject"
		"foo":      "extra", // no close match
	})
	if result != nil {
		t.Fatalf("expected no error (required params present), got %+v", result)
	}
	if len(warns) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warns), warns)
	}
	// One should suggest "subject", the other should say "ignored".
	var hasSuggestion, hasIgnored bool
	for _, w := range warns {
		if strings.Contains(w, "did you mean") && strings.Contains(w, "subject") {
			hasSuggestion = true
		}
		if strings.Contains(w, "ignored") {
			hasIgnored = true
		}
	}
	if !hasSuggestion {
		t.Errorf("expected a typo suggestion warning, got %v", warns)
	}
	if !hasIgnored {
		t.Errorf("expected an ignored-param warning, got %v", warns)
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"subject", "subjectt", 1},
		{"recipent", "recipient", 1},
		{"to", "body", 3},
	}
	for _, c := range cases {
		got := editDistance(c.a, c.b)
		if got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
