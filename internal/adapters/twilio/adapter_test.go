package twilio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ericlevine/clawvisor/internal/adapters"
)

func validCred(token string) []byte {
	b, _ := json.Marshal(storedCredential{Type: "api_key", Token: token})
	return b
}

// ── ValidateCredential ────────────────────────────────────────────────────────

func TestValidateCredential_Valid(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("ACtest123:authtoken456")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateCredential_MissingColon(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("ACtest123authtoken456")); err == nil {
		t.Fatal("expected error for missing colon separator")
	}
}

func TestValidateCredential_WrongPrefix(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("XX123:authtoken456")); err == nil {
		t.Fatal("expected error for non-AC prefix")
	}
}

func TestValidateCredential_EmptyAuthToken(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("ACtest123:")); err == nil {
		t.Fatal("expected error for empty auth token")
	}
}

func TestValidateCredential_EmptySID(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred(":authtoken456")); err == nil {
		t.Fatal("expected error for empty SID")
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

// ── splitCredential ───────────────────────────────────────────────────────────

func TestSplitCredential(t *testing.T) {
	tests := []struct {
		input     string
		wantSID   string
		wantToken string
		wantErr   bool
	}{
		{"ACabc123:mytoken", "ACabc123", "mytoken", false},
		{"ACfoo:bar:baz", "ACfoo", "bar:baz", false}, // extra colons in token
		{"nocolon", "", "", true},
		{":token", "", "", true},
		{"sid:", "", "", true},
		{"", "", "", true},
	}
	for _, tt := range tests {
		sid, token, err := splitCredential(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("splitCredential(%q) expected error, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCredential(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if sid != tt.wantSID {
			t.Errorf("splitCredential(%q) sid = %q, want %q", tt.input, sid, tt.wantSID)
		}
		if token != tt.wantToken {
			t.Errorf("splitCredential(%q) token = %q, want %q", tt.input, token, tt.wantToken)
		}
	}
}

// ── ServiceID / SupportedActions ──────────────────────────────────────────────

func TestServiceID(t *testing.T) {
	if got := New().ServiceID(); got != "twilio" {
		t.Fatalf("expected 'twilio', got %q", got)
	}
}

func TestSupportedActions(t *testing.T) {
	actions := New().SupportedActions()
	expected := map[string]bool{
		"send_sms":       true,
		"send_whatsapp":  true,
		"list_messages":  true,
		"get_message":    true,
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
		Credential: validCred("ACtest:token"),
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

// ── Basic auth ────────────────────────────────────────────────────────────────

func TestBasicAuthTransport(t *testing.T) {
	var gotUser, gotPass string
	var gotBasicOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotBasicOK = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"sid": "SM123"})
	}))
	defer srv.Close()

	client := twilioClient("ACmysid", "mytoken")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	client.Do(req)

	if !gotBasicOK {
		t.Fatal("expected basic auth to be present")
	}
	if gotUser != "ACmysid" {
		t.Errorf("expected user 'ACmysid', got %q", gotUser)
	}
	if gotPass != "mytoken" {
		t.Errorf("expected pass 'mytoken', got %q", gotPass)
	}
}

// ── send_sms with test server ─────────────────────────────────────────────────

func TestSendSMS_Success(t *testing.T) {
	var gotTo, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		r.ParseForm()
		gotTo = r.FormValue("To")
		gotBody = r.FormValue("Body")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sid":    "SM123",
			"status": "queued",
			"to":     gotTo,
			"from":   "+15551234567",
		})
	}))
	defer srv.Close()

	a := New()
	sid := "ACtest123"
	client := twilioClient(sid, "authtoken")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL, sid: sid}

	result, err := a.sendSMS(context.Background(), client, sid, map[string]any{
		"to":   "+15559876543",
		"body": "Hello from Clawvisor",
		"from": "+15551234567",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("expected form-encoded, got %q", gotContentType)
	}
	if gotTo != "+15559876543" {
		t.Errorf("expected To '+15559876543', got %q", gotTo)
	}
	if gotBody != "Hello from Clawvisor" {
		t.Errorf("expected Body 'Hello from Clawvisor', got %q", gotBody)
	}
	if result.Summary != "SMS sent to +15559876543 (status: queued)" {
		t.Errorf("unexpected summary: %q", result.Summary)
	}
}

func TestSendSMS_MissingParams(t *testing.T) {
	a := New()
	_, err := a.sendSMS(context.Background(), &http.Client{}, "AC123", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing to and body")
	}
}

// ── send_whatsapp prefixes ────────────────────────────────────────────────────

func TestSendWhatsApp_AddsPrefix(t *testing.T) {
	var gotTo, gotFrom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotTo = r.FormValue("To")
		gotFrom = r.FormValue("From")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sid":    "SM456",
			"status": "queued",
			"to":     gotTo,
			"from":   gotFrom,
		})
	}))
	defer srv.Close()

	a := New()
	sid := "ACtest123"
	client := twilioClient(sid, "authtoken")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL, sid: sid}

	_, err := a.sendWhatsApp(context.Background(), client, sid, map[string]any{
		"to":   "+15559876543",
		"body": "Hello via WhatsApp",
		"from": "+15551234567",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTo != "whatsapp:+15559876543" {
		t.Errorf("expected 'whatsapp:+15559876543', got %q", gotTo)
	}
	if gotFrom != "whatsapp:+15551234567" {
		t.Errorf("expected 'whatsapp:+15551234567', got %q", gotFrom)
	}
}

func TestSendWhatsApp_AlreadyPrefixed(t *testing.T) {
	var gotTo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotTo = r.FormValue("To")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sid": "SM789", "status": "queued", "to": gotTo, "from": "+15551234567",
		})
	}))
	defer srv.Close()

	a := New()
	sid := "ACtest123"
	client := twilioClient(sid, "authtoken")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL, sid: sid}

	_, err := a.sendWhatsApp(context.Background(), client, sid, map[string]any{
		"to":   "whatsapp:+15559876543",
		"body": "Already prefixed",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should not double-prefix
	if gotTo != "whatsapp:+15559876543" {
		t.Errorf("expected 'whatsapp:+15559876543', got %q", gotTo)
	}
}

func TestSendWhatsApp_MissingParams(t *testing.T) {
	a := New()
	_, err := a.sendWhatsApp(context.Background(), &http.Client{}, "AC123", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing to and body")
	}
}

// ── get_message ───────────────────────────────────────────────────────────────

func TestGetMessage_MissingSID(t *testing.T) {
	a := New()
	_, err := a.getMessage(context.Background(), &http.Client{}, "AC123", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing sid")
	}
}

// ── HTTP error handling ───────────────────────────────────────────────────────

func TestSendSMS_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message": "invalid phone number"}`))
	}))
	defer srv.Close()

	a := New()
	sid := "ACtest123"
	client := twilioClient(sid, "authtoken")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL, sid: sid}

	_, err := a.sendSMS(context.Background(), client, sid, map[string]any{
		"to":   "invalid",
		"body": "test",
	})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

// rewriteTransport redirects Twilio API requests to the test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
	sid    string
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
