package yamlruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

func intPtr(v int) *int { return &v }

func testCred(token string) []byte {
	b, _ := json.Marshal(credential{Type: "api_key", Token: token})
	return b
}

func TestYAMLAdapter_RESTListAction(t *testing.T) {
	// Mock API server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/customers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "10" {
			t.Errorf("expected limit=10, got %s", r.URL.Query().Get("limit"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk_test" {
			t.Errorf("expected Bearer sk_test, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "cus_1", "email": "a@b.com", "name": "Alice", "created": 1234567890},
				{"id": "cus_2", "email": "c@d.com", "name": "Bob", "created": 1234567891},
			},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "stripe", DisplayName: "Stripe"},
		Auth: yamldef.AuthDef{
			Type:         "api_key",
			Header:       "Authorization",
			HeaderPrefix: "Bearer ",
		},
		API: yamldef.APIDef{BaseURL: srv.URL + "/v1", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_customers": {
				DisplayName: "List customers",
				Method:      "GET",
				Path:        "/customers",
				Params: map[string]yamldef.Param{
					"limit": {Type: "int", Default: 25, Max: intPtr(100), Location: "query"},
				},
				Response: yamldef.ResponseDef{
					DataPath: "data",
					Fields: []yamldef.FieldDef{
						{Name: "id"},
						{Name: "email"},
						{Name: "name", Sanitize: true},
						{Name: "created"},
					},
					Summary: "{{len .Data}} customer(s)",
				},
			},
		},
	}

	adapter := New(def, nil)

	if adapter.ServiceID() != "stripe" {
		t.Fatalf("expected service ID 'stripe', got %q", adapter.ServiceID())
	}

	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "list_customers",
		Params:     map[string]any{"limit": 10},
		Credential: testCred("sk_test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Summary != "2 customer(s)" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}

	items, ok := result.Data.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any, got %T", result.Data)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0]["id"] != "cus_1" {
		t.Errorf("expected cus_1, got %v", items[0]["id"])
	}
}

func TestYAMLAdapter_RESTGetWithPathParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/customers/cus_abc" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "cus_abc", "email": "test@example.com", "name": "Test User",
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "stripe"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL + "/v1", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"get_customer": {
				Method: "GET",
				Path:   "/customers/{{.customer_id}}",
				Params: map[string]yamldef.Param{
					"customer_id": {Type: "string", Required: true, Location: "path"},
				},
				Response: yamldef.ResponseDef{
					Fields: []yamldef.FieldDef{
						{Name: "id"},
						{Name: "email"},
						{Name: "name"},
					},
					Summary: "Customer {{.id}}: {{.email}}",
				},
			},
		},
	}

	adapter := New(def, nil)
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "get_customer",
		Params:     map[string]any{"customer_id": "cus_abc"},
		Credential: testCred("sk_test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Summary != "Customer cus_abc: test@example.com" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}
}

func TestYAMLAdapter_RESTFormPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("expected form content type, got %s", ct)
		}
		r.ParseForm()
		if r.FormValue("charge") != "ch_123" {
			t.Errorf("expected charge=ch_123, got %s", r.FormValue("charge"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "re_1", "amount": 1000, "currency": "usd", "charge": "ch_123", "status": "succeeded",
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "stripe"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL + "/v1", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"create_refund": {
				Method:   "POST",
				Path:     "/refunds",
				Encoding: "form",
				Params: map[string]yamldef.Param{
					"charge": {Type: "string", Required: true, Location: "body"},
					"amount": {Type: "int", Location: "body"},
				},
				Response: yamldef.ResponseDef{
					Fields: []yamldef.FieldDef{
						{Name: "id"},
						{Name: "amount", Transform: "money"},
						{Name: "currency"},
					},
					Summary: "Refund {{.id}}",
				},
			},
		},
	}

	adapter := New(def, nil)
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "create_refund",
		Params:     map[string]any{"charge": "ch_123", "amount": 1000},
		Credential: testCred("sk_test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result.Data)
	}
	// amount should be transformed from 1000 cents to "10.00"
	if data["amount"] != "10.00" {
		t.Errorf("expected amount '10.00', got %v", data["amount"])
	}
}

func TestYAMLAdapter_GraphQLAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if _, ok := payload["query"].(string); !ok {
			t.Errorf("expected query string in payload")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{"id": "1", "identifier": "LIN-1", "title": "Bug fix", "state": map[string]any{"name": "In Progress"}},
						{"id": "2", "identifier": "LIN-2", "title": "Feature", "state": map[string]any{"name": "Done"}},
					},
				},
			},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "linear"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: ""},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "graphql"},
		Actions: map[string]yamldef.Action{
			"list_issues": {
				Query: `query($first: Int!) { issues(first: $first) { nodes { id identifier title state { name } } } }`,
				Params: map[string]yamldef.Param{
					"first": {Type: "int", Default: 50, Max: intPtr(250), GraphQLVar: true},
				},
				Response: yamldef.ResponseDef{
					DataPath: "data.issues.nodes",
					Fields: []yamldef.FieldDef{
						{Name: "id"},
						{Name: "identifier"},
						{Name: "title", Sanitize: true},
						{Name: "state", Path: "state.name"},
					},
					Summary: "{{len .Data}} issue(s)",
				},
			},
		},
	}

	adapter := New(def, nil)
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "list_issues",
		Params:     map[string]any{},
		Credential: testCred("lin_api_test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Summary != "2 issue(s)" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}

	items, ok := result.Data.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any, got %T", result.Data)
	}
	if items[0]["state"] != "In Progress" {
		t.Errorf("expected 'In Progress', got %v", items[0]["state"])
	}
}

func TestYAMLAdapter_BasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "AC123" || pass != "authtoken" {
			t.Errorf("expected basic auth AC123:authtoken, got %s:%s (ok=%v)", user, pass, ok)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"sid": "SM1", "status": "sent"})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "twilio"},
		Auth:    yamldef.AuthDef{Type: "basic"},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"send_sms": {
				Method: "POST",
				Path:   "/Messages.json",
				Params: map[string]yamldef.Param{
					"to":   {Type: "string", Required: true, Location: "body"},
					"body": {Type: "string", Required: true, Location: "body"},
				},
				Response: yamldef.ResponseDef{
					Fields:  []yamldef.FieldDef{{Name: "sid"}, {Name: "status"}},
					Summary: "SMS sent: {{.sid}}",
				},
			},
		},
	}

	adapter := New(def, nil)
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "send_sms",
		Params:     map[string]any{"to": "+1234", "body": "hello"},
		Credential: testCred("AC123:authtoken"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Summary != "SMS sent: SM1" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}
}

func TestYAMLAdapter_ErrorCheckSlackStyle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "channel_not_found",
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "slack"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_channels": {
				Method: "GET",
				Path:   "/conversations.list",
				ErrorCheck: &yamldef.ErrorCheckDef{
					SuccessPath: "ok",
					ErrorPath:   "error",
				},
				Response: yamldef.ResponseDef{
					DataPath: "channels",
					Summary:  "channels",
				},
			},
		},
	}

	adapter := New(def, nil)
	_, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "list_channels",
		Params:     map[string]any{},
		Credential: testCred("xoxb-test"),
	})
	if err == nil {
		t.Fatal("expected error for Slack-style error response")
	}
	if !contains(err.Error(), "channel_not_found") {
		t.Errorf("expected error to contain 'channel_not_found', got: %v", err)
	}
}

func TestYAMLAdapter_GoOverride(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: "http://unused", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"custom_action": {Override: "go"},
		},
	}

	overrides := map[string]ActionFunc{
		"custom_action": func(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
			return &adapters.Result{Summary: "override worked", Data: nil}, nil
		},
	}

	adapter := New(def, overrides)
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "custom_action",
		Params:     map[string]any{},
		Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Summary != "override worked" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}
}

func TestYAMLAdapter_ServiceMetadata(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID:          "stripe",
			DisplayName: "Stripe",
			Description: "Payment processing",
			SetupURL:    "https://example.com/keys",
		},
		Auth: yamldef.AuthDef{Type: "api_key"},
		API:  yamldef.APIDef{Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_customers": {
				DisplayName: "List customers",
				Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "medium", Description: "List Stripe customers"},
			},
		},
	}

	adapter := New(def, nil)
	meta := adapter.ServiceMetadata()

	if meta.DisplayName != "Stripe" {
		t.Errorf("expected Stripe, got %q", meta.DisplayName)
	}
	if meta.SetupURL != "https://example.com/keys" {
		t.Errorf("unexpected setup URL: %q", meta.SetupURL)
	}
	am, ok := meta.ActionMeta["list_customers"]
	if !ok {
		t.Fatal("expected list_customers in action meta")
	}
	if am.Category != "read" || am.Sensitivity != "medium" {
		t.Errorf("unexpected action meta: %+v", am)
	}
}

func TestYAMLAdapter_ValidateCredential(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key"},
	}
	adapter := New(def, nil)

	if err := adapter.ValidateCredential(testCred("valid")); err != nil {
		t.Errorf("expected valid credential: %v", err)
	}
	if err := adapter.ValidateCredential(testCred("")); err == nil {
		t.Error("expected error for empty token")
	}
	if err := adapter.ValidateCredential(nil); err == nil {
		t.Error("expected error for nil credential")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}
func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
