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

func testOAuthCred(accessToken, refreshToken string) []byte {
	b, _ := json.Marshal(map[string]string{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
	})
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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
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

func TestYAMLAdapter_BasicAuthUserVar(t *testing.T) {
	// Twilio-style: the Account SID is collected as a non-secret variable
	// and used both as the basic-auth username and in base_url. The vaulted
	// credential is just the Auth Token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "AC123" || pass != "authtoken" {
			t.Errorf("expected basic auth AC123:authtoken, got %s:%s (ok=%v)", user, pass, ok)
		}
		if r.URL.Path != "/Accounts/AC123/Messages.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{}})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service:   yamldef.ServiceInfo{ID: "twilio"},
		Auth:      yamldef.AuthDef{Type: "basic", UserVar: "account_sid"},
		Variables: map[string]yamldef.VariableDef{"account_sid": {DisplayName: "Account SID", Required: true}},
		API:       yamldef.APIDef{BaseURL: srv.URL + "/Accounts/{{.var.account_sid}}", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_messages": {
				Method: "GET",
				Path:   "/Messages.json",
				Response: yamldef.ResponseDef{
					DataPath: "messages",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if _, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "list_messages",
		Config:     map[string]string{"account_sid": "AC123"},
		Credential: testCred("authtoken"),
	}); err != nil {
		t.Fatalf("Execute failed: %v", err)
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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = adapter.Execute(context.Background(), adapters.Request{
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

	adapter, err := New(def, overrides)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
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

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
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
	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

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

func TestYAMLAdapter_ValidateCredential_OAuth2(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "oauth2"},
	}
	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	if err := adapter.ValidateCredential(testOAuthCred("access", "")); err != nil {
		t.Fatalf("expected access-token oauth2 credential to validate: %v", err)
	}
	if err := adapter.ValidateCredential(testOAuthCred("", "refresh")); err != nil {
		t.Fatalf("expected refresh-token oauth2 credential to validate: %v", err)
	}
	if err := adapter.ValidateCredential(testOAuthCred("", "")); err == nil {
		t.Fatal("expected error for oauth2 credential with no tokens")
	}
}

func TestYAMLAdapter_ValidateCredential_APIKeyWithPKCECred(t *testing.T) {
	// api_key adapters with pkce_flow store credentials in OAuth2 format.
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key"},
	}
	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// PKCE-sourced credential has access_token, not token.
	if err := adapter.ValidateCredential(testOAuthCred("pkce-access-token", "")); err != nil {
		t.Errorf("expected PKCE credential (access_token) to validate for api_key adapter: %v", err)
	}
	if err := adapter.ValidateCredential(testOAuthCred("", "")); err == nil {
		t.Error("expected error for empty credential")
	}
}

// ── Expr-lang integration tests ──────────────────────────────────────────────

func TestYAMLAdapter_ExprFieldExtraction(t *testing.T) {
	// Simulate a Contacts-style API with nested arrays.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"connections": []map[string]any{
				{
					"resourceName":   "people/123",
					"names":          []map[string]any{{"displayName": "Alice Smith"}},
					"emailAddresses": []map[string]any{{"value": "alice@example.com"}},
					"phoneNumbers":   []map[string]any{{"value": "+1234567890"}},
				},
				{
					"resourceName":   "people/456",
					"names":          []map[string]any{{"displayName": "Bob Jones"}},
					"emailAddresses": []map[string]any{{"value": "bob@example.com"}},
					// No phone number — tests optional field omission.
				},
			},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "contacts"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list": {
				Method: "GET",
				Path:   "/connections",
				Response: yamldef.ResponseDef{
					DataPath: "connections",
					Fields: []yamldef.FieldDef{
						{Name: "id", Expr: "resourceName"},
						{Name: "name", Expr: "names[0]?.displayName ?? ''", Sanitize: true},
						{Name: "email", Expr: "emailAddresses[0]?.value ?? ''"},
						{Name: "phone", Expr: "phoneNumbers[0]?.value ?? ''", Optional: true},
					},
					Summary: "{{len .Data}} contact(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action: "list", Params: map[string]any{}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	items, ok := result.Data.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any, got %T", result.Data)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	if items[0]["id"] != "people/123" {
		t.Errorf("expected people/123, got %v", items[0]["id"])
	}
	if items[0]["name"] != "Alice Smith" {
		t.Errorf("expected Alice Smith, got %v", items[0]["name"])
	}
	if items[0]["email"] != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %v", items[0]["email"])
	}
	if items[0]["phone"] != "+1234567890" {
		t.Errorf("expected +1234567890, got %v", items[0]["phone"])
	}
	// Bob has no phone — optional field should be omitted.
	if _, hasPhone := items[1]["phone"]; hasPhone {
		t.Errorf("expected phone to be omitted for Bob, got %v", items[1]["phone"])
	}

	if result.Summary != "2 contact(s)" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}
}

func TestYAMLAdapter_ParamMapToAndTransform(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify map_to renamed the param.
		if r.URL.Query().Get("pageSize") != "5" {
			t.Errorf("expected pageSize=5, got %q", r.URL.Query().Get("pageSize"))
		}
		// Verify the original param name is NOT in the query.
		if r.URL.Query().Get("max_results") != "" {
			t.Errorf("original param name 'max_results' should not appear in query")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list": {
				Method: "GET", Path: "/items",
				Params: map[string]yamldef.Param{
					"max_results": {Type: "int", Default: 10, Max: intPtr(50), MapTo: "pageSize", Location: "query"},
				},
				Response: yamldef.ResponseDef{Summary: "done"},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action: "list", Params: map[string]any{"max_results": 5}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
}

func TestYAMLAdapter_SparseBodyMode(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "1", "summary": "Updated"})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"update": {
				Method: "PATCH", Path: "/items/{{.id}}",
				BodyMode: "sparse",
				Params: map[string]yamldef.Param{
					"id":      {Type: "string", Required: true, Location: "path"},
					"title":   {Type: "string", Location: "body"},
					"summary": {Type: "string", Location: "body"},
					"status":  {Type: "string", Location: "body"},
				},
				Response: yamldef.ResponseDef{
					Fields:  []yamldef.FieldDef{{Name: "id"}, {Name: "summary"}},
					Summary: "updated",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	// Only provide "title" — other body params should be omitted in sparse mode.
	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action:     "update",
		Params:     map[string]any{"id": "42", "title": "New Title"},
		Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if _, has := receivedBody["title"]; !has {
		t.Error("expected 'title' in body")
	}
	if _, has := receivedBody["summary"]; has {
		t.Error("'summary' should NOT be in sparse body (not provided)")
	}
	if _, has := receivedBody["status"]; has {
		t.Error("'status' should NOT be in sparse body (not provided)")
	}
}

func TestYAMLAdapter_ParamTransformExpr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timeMin := r.URL.Query().Get("timeMin")
		// The transform should have converted "2024-06-15" to RFC3339.
		if timeMin != "2024-06-15T00:00:00Z" {
			t.Errorf("expected RFC3339 timeMin, got %q", timeMin)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list": {
				Method: "GET", Path: "/events",
				Params: map[string]yamldef.Param{
					"from": {Type: "string", Transform: "rfc3339(from)", MapTo: "timeMin", Location: "query"},
				},
				Response: yamldef.ResponseDef{Summary: "done"},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action: "list", Params: map[string]any{"from": "2024-06-15"}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
}

func TestCustomFunctions(t *testing.T) {
	tests := []struct {
		name string
		fn   func(...any) (any, error)
		args []any
		want any
	}{
		{"rfc3339 passthrough", rfc3339Func, []any{"2024-01-01T00:00:00Z"}, "2024-01-01T00:00:00Z"},
		{"rfc3339 date only", rfc3339Func, []any{"2024-01-01"}, "2024-01-01T00:00:00Z"},
		{"rfc3339 empty", rfc3339Func, []any{""}, ""},
		{"endOfDay date", endOfDayFunc, []any{"2024-01-01"}, "2024-01-01T23:59:59Z"},
		{"endOfDay passthrough", endOfDayFunc, []any{"2024-01-01T12:00:00Z"}, "2024-01-01T12:00:00Z"},
		{"isAllDay true", isAllDayFunc, []any{"2024-01-01"}, true},
		{"isAllDay false", isAllDayFunc, []any{"2024-01-01T12:00:00Z"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.fn(tt.args...)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGmailExprFunctions(t *testing.T) {
	t.Run("findHeader", func(t *testing.T) {
		headers := []any{
			map[string]any{"name": "From", "value": "alice@example.com"},
			map[string]any{"name": "Subject", "value": "Hello"},
			map[string]any{"name": "Date", "value": "Mon, 1 Jan 2024"},
		}
		got, err := findHeaderFunc(headers, "from") // case-insensitive
		if err != nil {
			t.Fatal(err)
		}
		if got != "alice@example.com" {
			t.Errorf("got %q, want alice@example.com", got)
		}

		got, _ = findHeaderFunc(headers, "X-Missing")
		if got != "" {
			t.Errorf("missing header: got %q, want empty", got)
		}
	})

	t.Run("base64Decode", func(t *testing.T) {
		// URL-safe base64 for "Hello, World!"
		got, err := base64DecodeFunc("SGVsbG8sIFdvcmxkIQ==")
		if err != nil {
			t.Fatal(err)
		}
		if got != "Hello, World!" {
			t.Errorf("got %q, want %q", got, "Hello, World!")
		}
	})

	t.Run("stripHTML", func(t *testing.T) {
		html := "<html><body><style>body{color:red}</style><p>Hello <b>world</b></p><script>alert(1)</script></body></html>"
		got, err := stripHTMLFunc(html)
		if err != nil {
			t.Fatal(err)
		}
		s := got.(string)
		if !contains(s, "Hello") || !contains(s, "world") {
			t.Errorf("expected 'Hello' and 'world' in %q", s)
		}
		if contains(s, "<") || contains(s, "script") || contains(s, "style") {
			t.Errorf("HTML not stripped: %q", s)
		}
	})

	t.Run("extractMimeBody_plaintext", func(t *testing.T) {
		// Simulate a Gmail payload with direct text/plain body.
		payload := map[string]any{
			"mimeType": "text/plain",
			"body":     map[string]any{"data": "SGVsbG8gd29ybGQ="}, // "Hello world"
		}
		got, err := extractMimeBodyFunc(payload)
		if err != nil {
			t.Fatal(err)
		}
		if got != "Hello world" {
			t.Errorf("got %q, want %q", got, "Hello world")
		}
	})

	t.Run("extractMimeBody_multipart_plain", func(t *testing.T) {
		// Multipart message with text/plain part.
		payload := map[string]any{
			"mimeType": "multipart/alternative",
			"body":     map[string]any{},
			"parts": []any{
				map[string]any{
					"mimeType": "text/html",
					"body":     map[string]any{"data": "PHA+SFRNTDWVCD4="}, // "<p>HTML</p>"
				},
				map[string]any{
					"mimeType": "text/plain",
					"body":     map[string]any{"data": "UGxhaW4gdGV4dA=="}, // "Plain text"
				},
			},
		}
		got, _ := extractMimeBodyFunc(payload)
		if got != "Plain text" {
			t.Errorf("got %q, want %q", got, "Plain text")
		}
	})

	t.Run("extractMimeBody_html_fallback", func(t *testing.T) {
		// Only HTML part — should strip HTML.
		payload := map[string]any{
			"mimeType": "multipart/alternative",
			"body":     map[string]any{},
			"parts": []any{
				map[string]any{
					"mimeType": "text/html",
					"body":     map[string]any{"data": "PHA+SGVsbG88L3A+"}, // "<p>Hello</p>"
				},
			},
		}
		got, _ := extractMimeBodyFunc(payload)
		s := got.(string)
		if !contains(s, "Hello") {
			t.Errorf("expected 'Hello' in %q", s)
		}
		if contains(s, "<p>") {
			t.Errorf("HTML not stripped: %q", s)
		}
	})

	t.Run("extractMimeBody_nested_multipart", func(t *testing.T) {
		// Nested multipart — text/plain is 2 levels deep.
		payload := map[string]any{
			"mimeType": "multipart/mixed",
			"body":     map[string]any{},
			"parts": []any{
				map[string]any{
					"mimeType": "multipart/alternative",
					"body":     map[string]any{},
					"parts": []any{
						map[string]any{
							"mimeType": "text/plain",
							"body":     map[string]any{"data": "TmVzdGVk"}, // "Nested"
						},
					},
				},
			},
		}
		got, _ := extractMimeBodyFunc(payload)
		if got != "Nested" {
			t.Errorf("got %q, want %q", got, "Nested")
		}
	})
}

// TestYAMLAdapter_GmailGetMessage tests the full get_message action with a mock Gmail API.
func TestYAMLAdapter_GmailGetMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":       "msg123",
			"threadId": "thread456",
			"snippet":  "Hey there...",
			"labelIds": []string{"INBOX", "UNREAD"},
			"payload": map[string]any{
				"mimeType": "multipart/alternative",
				"headers": []map[string]any{
					{"name": "From", "value": "alice@example.com"},
					{"name": "To", "value": "bob@example.com"},
					{"name": "Subject", "value": "Test Subject"},
					{"name": "Date", "value": "Mon, 1 Jan 2024 12:00:00 +0000"},
				},
				"body": map[string]any{},
				"parts": []map[string]any{
					{
						"mimeType": "text/plain",
						"body":     map[string]any{"data": "SGVsbG8gZnJvbSBHbWFpbA=="}, // "Hello from Gmail"
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test.gmail"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"get_message": {
				Method: "GET",
				Path:   "/users/me/messages/{{.message_id}}",
				Params: map[string]yamldef.Param{
					"message_id": {Type: "string", Required: true, Location: "path"},
					"format":     {Type: "string", Default: "full", Location: "query"},
				},
				Response: yamldef.ResponseDef{
					Fields: []yamldef.FieldDef{
						{Name: "id"},
						{Name: "from", Expr: "findHeader(payload.headers, 'From')", Sanitize: true},
						{Name: "to", Expr: "findHeader(payload.headers, 'To')", Sanitize: true},
						{Name: "subject", Expr: "findHeader(payload.headers, 'Subject')", Sanitize: true},
						{Name: "date", Expr: "findHeader(payload.headers, 'Date')"},
						{Name: "body", Expr: "sanitize(extractMimeBody(payload) != '' ? extractMimeBody(payload) : snippet, 2000)"},
						{Name: "is_unread", Expr: "'UNREAD' in labelIds"},
						{Name: "threadId", Rename: "thread_id"},
					},
					Summary: "Email from {{.from}}: {{.subject}}",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action:     "get_message",
		Params:     map[string]any{"message_id": "msg123"},
		Credential: testCred("test-token"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result.Data)
	}

	checks := map[string]string{
		"id":        "msg123",
		"from":      "alice@example.com",
		"to":        "bob@example.com",
		"subject":   "Test Subject",
		"date":      "Mon, 1 Jan 2024 12:00:00 +0000",
		"body":      "Hello from Gmail",
		"thread_id": "thread456",
	}
	for key, want := range checks {
		got, _ := data[key].(string)
		if got != want {
			t.Errorf("%s: got %q, want %q", key, got, want)
		}
	}

	isUnread, ok := data["is_unread"].(bool)
	if !ok || !isUnread {
		t.Errorf("is_unread: got %v, want true", data["is_unread"])
	}

	if !contains(result.Summary, "alice@example.com") {
		t.Errorf("summary missing sender: %q", result.Summary)
	}
}

func TestYAMLAdapter_PathParamDefault(t *testing.T) {
	// Regression: when a path param has a default and the caller omits it,
	// the placeholder must still be replaced (was causing 404s for Calendar).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/calendars/primary/events" {
			t.Errorf("unexpected path: %s (want /v3/calendars/primary/events)", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []any{},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "google.calendar"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL + "/v3", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_events": {
				Method: "GET",
				Path:   "/calendars/{{.calendar_id}}/events",
				Params: map[string]yamldef.Param{
					"calendar_id": {Type: "string", Default: "primary", Location: "path"},
					"max_results": {Type: "int", Default: 10, MapTo: "maxResults", Location: "query"},
				},
				Response: yamldef.ResponseDef{
					DataPath: "items",
					Summary:  "{{len .Data}} event(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	// Call WITHOUT calendar_id — should default to "primary".
	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action:     "list_events",
		Params:     map[string]any{},
		Credential: testCred("test-token"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
}

func TestYAMLAdapter_PathParamEscapesReservedChars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v3/calendars/en.usa%23holiday@group.v.calendar.google.com/events"
		if r.URL.EscapedPath() != want {
			t.Errorf("unexpected escaped path: %s (want %s)", r.URL.EscapedPath(), want)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []any{},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "google.calendar"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL + "/v3", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_events": {
				Method: "GET",
				Path:   "/calendars/{{.calendar_id}}/events",
				Params: map[string]yamldef.Param{
					"calendar_id": {Type: "string", Default: "primary", Location: "path"},
				},
				Response: yamldef.ResponseDef{
					DataPath: "items",
					Summary:  "{{len .Data}} event(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action: "list_events",
		Params: map[string]any{
			"calendar_id": "en.usa#holiday@group.v.calendar.google.com",
		},
		Credential: testCred("test-token"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
}

func TestYAMLAdapter_PathParamPreservesSlashes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v1/people/123"
		if r.URL.EscapedPath() != want {
			t.Errorf("unexpected escaped path: %s (want %s)", r.URL.EscapedPath(), want)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"resourceName": "people/123",
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "google.contacts"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL + "/v1", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"get_contact": {
				Method: "GET",
				Path:   "/{{.contact_id}}",
				Params: map[string]yamldef.Param{
					"contact_id": {Type: "string", Required: true, Location: "path"},
				},
				Response: yamldef.ResponseDef{
					Fields: []yamldef.FieldDef{
						{Name: "resourceName"},
					},
					Summary: "Contact {{.resourceName}}",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action: "get_contact",
		Params: map[string]any{
			"contact_id": "people/123",
		},
		Credential: testCred("test-token"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
}

func TestYAMLAdapter_UnresolvedPathParam(t *testing.T) {
	// When a required path param is missing and has no default, the error
	// should mention the parameter name, not silently send a broken URL.
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test.svc"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: "http://unused", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"get_item": {
				Method: "GET",
				Path:   "/items/{{.item_id}}",
				Params: map[string]yamldef.Param{
					"item_id": {Type: "string", Location: "path"},
				},
				Response: yamldef.ResponseDef{},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = adapter.Execute(context.Background(), adapters.Request{
		Action:     "get_item",
		Params:     map[string]any{},
		Credential: testCred("test-token"),
	})
	if err == nil {
		t.Fatal("expected error for missing path param, got nil")
	}
	if !contains(err.Error(), "item_id") {
		t.Errorf("error should mention 'item_id', got: %s", err.Error())
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}
func TestYAMLAdapter_ResponseMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files":         []any{map[string]any{"id": "1", "name": "test.txt"}},
			"nextPageToken": "token_abc",
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list": {
				Method: "GET", Path: "/files",
				Params: map[string]yamldef.Param{
					"page_size": {Type: "int", Default: 10, Location: "query"},
				},
				Response: yamldef.ResponseDef{
					DataPath: "files",
					Fields:   []yamldef.FieldDef{{Name: "id"}, {Name: "name"}},
					Meta: []yamldef.MetaDef{
						{Name: "nextPageToken", Rename: "next_page_token"},
					},
					Summary: "{{len .Data}} file(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action: "list", Params: map[string]any{}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Meta == nil {
		t.Fatal("expected non-nil Meta")
	}
	if result.Meta["next_page_token"] != "token_abc" {
		t.Errorf("expected meta next_page_token=token_abc, got %v", result.Meta["next_page_token"])
	}

	// Verify Data is still correct.
	items, ok := result.Data.([]map[string]any)
	if !ok {
		t.Fatalf("expected Data to be []map[string]any, got %T", result.Data)
	}
	if len(items) != 1 || items[0]["id"] != "1" {
		t.Errorf("unexpected data: %v", items)
	}
}

func TestYAMLAdapter_ResponseMetaNested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"channels": []any{map[string]any{"id": "C1", "name": "general"}},
			"response_metadata": map[string]any{
				"next_cursor": "cursor_xyz",
			},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list": {
				Method: "GET", Path: "/channels",
				Response: yamldef.ResponseDef{
					DataPath: "channels",
					Fields:   []yamldef.FieldDef{{Name: "id"}, {Name: "name"}},
					Meta: []yamldef.MetaDef{
						{Name: "response_metadata.next_cursor", Rename: "next_cursor"},
					},
					Summary: "{{len .Data}} channel(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action: "list", Params: map[string]any{}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Meta == nil {
		t.Fatal("expected non-nil Meta")
	}
	if result.Meta["next_cursor"] != "cursor_xyz" {
		t.Errorf("expected meta next_cursor=cursor_xyz, got %v", result.Meta["next_cursor"])
	}
}

func TestYAMLAdapter_ResponseMetaGraphQL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []any{
						map[string]any{"id": "1", "title": "Bug"},
					},
					"pageInfo": map[string]any{
						"hasNextPage": true,
						"endCursor":   "cursor_end",
					},
				},
			},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "graphql"},
		Actions: map[string]yamldef.Action{
			"list_issues": {
				Query: `query($first: Int!) { issues(first: $first) { nodes { id title } pageInfo { hasNextPage endCursor } } }`,
				Params: map[string]yamldef.Param{
					"first": {Type: "int", Default: 50, GraphQLVar: true},
				},
				Response: yamldef.ResponseDef{
					DataPath: "data.issues.nodes",
					Fields:   []yamldef.FieldDef{{Name: "id"}, {Name: "title"}},
					Meta: []yamldef.MetaDef{
						{Name: "data.issues.pageInfo.hasNextPage", Rename: "has_more"},
						{Name: "data.issues.pageInfo.endCursor", Rename: "end_cursor"},
					},
					Summary: "{{len .Data}} issue(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action: "list_issues", Params: map[string]any{}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if result.Meta == nil {
		t.Fatal("expected non-nil Meta")
	}
	if result.Meta["has_more"] != true {
		t.Errorf("expected meta has_more=true, got %v", result.Meta["has_more"])
	}
	if result.Meta["end_cursor"] != "cursor_end" {
		t.Errorf("expected meta end_cursor=cursor_end, got %v", result.Meta["end_cursor"])
	}
}

func TestYAMLAdapter_ResponseMetaOmittedWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"files": []any{map[string]any{"id": "1"}},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "test"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list": {
				Method: "GET", Path: "/files",
				Response: yamldef.ResponseDef{
					DataPath: "files",
					Fields:   []yamldef.FieldDef{{Name: "id"}},
					Meta: []yamldef.MetaDef{
						{Name: "nextPageToken", Rename: "next_page_token"},
					},
					Summary: "{{len .Data}} file(s)",
				},
			},
		},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	result, err := adapter.Execute(context.Background(), adapters.Request{
		Action: "list", Params: map[string]any{}, Credential: testCred("test"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// When the API doesn't return the meta field, Meta should be nil.
	if result.Meta != nil {
		t.Errorf("expected nil Meta when no pagination token, got %v", result.Meta)
	}

	// Verify JSON serialization omits meta.
	b, _ := json.Marshal(result)
	if containsHelper(string(b), "meta") {
		t.Errorf("expected 'meta' to be omitted from JSON, got %s", string(b))
	}
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
