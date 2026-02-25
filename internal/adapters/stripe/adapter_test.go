package stripe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/adapters"
)

func validCred(token string) []byte {
	b, _ := json.Marshal(storedCredential{Type: "api_key", Token: token})
	return b
}

// ── ValidateCredential ────────────────────────────────────────────────────────

func TestValidateCredential_Valid(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("sk_test_abc123")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateCredential_RestrictedKey(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("rk_test_abc123")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateCredential_EmptyToken(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("")); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestValidateCredential_InvalidJSON(t *testing.T) {
	a := New()
	if err := a.ValidateCredential([]byte("bad")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ── ServiceID / SupportedActions ──────────────────────────────────────────────

func TestServiceID(t *testing.T) {
	if got := New().ServiceID(); got != "stripe" {
		t.Fatalf("expected 'stripe', got %q", got)
	}
}

func TestSupportedActions(t *testing.T) {
	actions := New().SupportedActions()
	expected := map[string]bool{
		"list_customers":    true,
		"get_customer":      true,
		"list_charges":      true,
		"get_charge":        true,
		"list_subscriptions": true,
		"get_subscription":  true,
		"create_refund":     true,
		"get_balance":       true,
	}
	if len(actions) != len(expected) {
		t.Fatalf("expected %d actions, got %d", len(expected), len(actions))
	}
	for _, a := range actions {
		if !expected[a] {
			t.Errorf("unexpected action %q", a)
		}
	}
}

// ── Execute unknown action ────────────────────────────────────────────────────

func TestExecute_UnknownAction(t *testing.T) {
	_, err := New().Execute(context.Background(), adapters.Request{
		Action:     "nonexistent",
		Params:     map[string]any{},
		Credential: validCred("sk_test_abc"),
	})
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

// ── OAuth stubs ───────────────────────────────────────────────────────────────

func TestOAuthStubs(t *testing.T) {
	a := New()
	if a.OAuthConfig() != nil {
		t.Error("OAuthConfig should return nil")
	}
	if a.RequiredScopes() != nil {
		t.Error("RequiredScopes should return nil")
	}
	if _, err := a.CredentialFromToken(nil); err == nil {
		t.Error("CredentialFromToken should return error")
	}
}

// ── formatMoney ───────────────────────────────────────────────────────────────

func TestFormatMoney(t *testing.T) {
	tests := []struct {
		amount   int64
		currency string
		want     string
	}{
		{2000, "usd", "20.00"},
		{0, "usd", "0.00"},
		{99, "eur", "0.99"},
		{100050, "usd", "1000.50"},
		{-500, "usd", "-5.00"},
		{1, "usd", "0.01"},
	}
	for _, tt := range tests {
		got := formatMoney(tt.amount, tt.currency)
		if got != tt.want {
			t.Errorf("formatMoney(%d, %q) = %q, want %q", tt.amount, tt.currency, got, tt.want)
		}
	}
}

// ── Auth header ───────────────────────────────────────────────────────────────

func TestTokenTransport_SetsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	client := stripeClient("sk_test_abc")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	client.Do(req)

	if gotAuth != "Bearer sk_test_abc" {
		t.Errorf("expected 'Bearer sk_test_abc', got %q", gotAuth)
	}
}

// ── list_customers with test server ───────────────────────────────────────────

func TestListCustomers_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "cus_1", "email": "alice@example.com", "name": "Alice", "created": 1700000000},
				{"id": "cus_2", "email": "bob@example.com", "name": "Bob", "created": 1700000001},
			},
		})
	}))
	defer srv.Close()

	a := New()
	client := stripeClient("sk_test_abc")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	result, err := a.listCustomers(context.Background(), client, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "2 customer(s)" {
		t.Errorf("expected '2 customer(s)', got %q", result.Summary)
	}
}

// ── create_refund with form-encoded POST ──────────────────────────────────────

func TestCreateRefund_FormEncoded(t *testing.T) {
	var gotContentType string
	var gotCharge string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		r.ParseForm()
		gotCharge = r.FormValue("charge")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":       "re_1",
			"amount":   1000,
			"currency": "usd",
			"charge":   gotCharge,
			"status":   "succeeded",
		})
	}))
	defer srv.Close()

	a := New()
	client := stripeClient("sk_test_abc")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	result, err := a.createRefund(context.Background(), client, map[string]any{
		"charge": "ch_abc123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("expected form-encoded content type, got %q", gotContentType)
	}
	if gotCharge != "ch_abc123" {
		t.Errorf("expected charge 'ch_abc123', got %q", gotCharge)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data, got %T", result.Data)
	}
	if data["amount"] != "10.00" {
		t.Errorf("expected amount '10.00', got %v", data["amount"])
	}
}

func TestCreateRefund_MissingCharge(t *testing.T) {
	a := New()
	_, err := a.createRefund(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing charge")
	}
}

// ── get_balance with test server ──────────────────────────────────────────────

func TestGetBalance_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"available": []map[string]any{
				{"amount": 50000, "currency": "usd"},
			},
			"pending": []map[string]any{
				{"amount": 1000, "currency": "usd"},
			},
		})
	}))
	defer srv.Close()

	a := New()
	client := stripeClient("sk_test_abc")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	result, err := a.getBalance(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Balance: 500.00 USD available" {
		t.Errorf("expected 'Balance: 500.00 USD available', got %q", result.Summary)
	}
}

// ── Required params validation ────────────────────────────────────────────────

func TestGetCustomer_MissingID(t *testing.T) {
	a := New()
	_, err := a.getCustomer(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing customer_id")
	}
}

func TestGetCharge_MissingID(t *testing.T) {
	a := New()
	_, err := a.getCharge(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing charge_id")
	}
}

func TestGetSubscription_MissingID(t *testing.T) {
	a := New()
	_, err := a.getSubscription(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing subscription_id")
	}
}

// ── HTTP error handling ───────────────────────────────────────────────────────

func TestListCustomers_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API Key"}}`))
	}))
	defer srv.Close()

	a := New()
	client := stripeClient("sk_test_bad")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	_, err := a.listCustomers(context.Background(), client, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

// rewriteTransport redirects all requests to the test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	parsed, _ := req.URL.Parse(t.target + req.URL.Path)
	if req.URL.RawQuery != "" {
		parsed.RawQuery = req.URL.RawQuery
	}
	clone.URL = parsed
	return t.base.RoundTrip(clone)
}
