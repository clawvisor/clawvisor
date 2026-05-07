package outlook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/adapters/microsoft"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

type mockOAuthProvider struct{}

func (m mockOAuthProvider) OAuthClientCredentials() (clientID, clientSecret string) {
	return "client_id", "client_secret"
}

func mockCredential() []byte {
	c := microsoft.Stored{
		Type:         "oauth2",
		AccessToken:  "token123",
		RefreshToken: "refresh123",
		Expiry:       time.Now().Add(1 * time.Hour),
		Scopes:       []string{"scope1"},
	}
	b, _ := json.Marshal(c)
	return b
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestExecute_InvalidToken(t *testing.T) {
	a := New(mockOAuthProvider{})
	_, err := a.Execute(context.Background(), adapters.Request{
		Action:     "send_message",
		Credential: []byte(`{"invalid": true}`),
	})
	if err == nil {
		t.Errorf("Expected error for invalid token, got nil")
	}
}

func TestExecute_UnsupportedAction(t *testing.T) {
	a := New(mockOAuthProvider{})
	_, err := a.Execute(context.Background(), adapters.Request{
		Action:     "unknown_action",
		Credential: mockCredential(),
	})
	if err == nil {
		t.Errorf("Expected error for unsupported action, got nil")
	}
}

func TestSendMessage(t *testing.T) {
	var sentPayload map[string]any

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/me/sendMail") {
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if err := json.Unmarshal(data, &sentPayload); err != nil {
				t.Fatalf("unmarshal send payload: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		}),
	}

	adapter := &Adapter{}
	result, err := adapter.sendMessage(context.Background(), client, map[string]any{
		"to":      "test@example.com",
		"subject": "Hello Graph",
		"body":    "This is a test message.",
	})
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if result == nil {
		t.Fatal("sendMessage returned nil result")
	}

	msg, ok := sentPayload["message"].(map[string]any)
	if !ok {
		t.Fatalf("missing message object in payload")
	}
	if msg["subject"] != "Hello Graph" {
		t.Errorf("expected subject 'Hello Graph', got %v", msg["subject"])
	}
	body, ok := msg["body"].(map[string]any)
	if !ok {
		t.Fatalf("missing body object in message")
	}
	if body["content"] != "This is a test message." {
		t.Errorf("expected body content 'This is a test message.', got %v", body["content"])
	}
	recipients, ok := msg["toRecipients"].([]any)
	if !ok || len(recipients) != 1 {
		t.Fatalf("missing or invalid toRecipients array")
	}
	recipient := recipients[0].(map[string]any)
	emailAddress := recipient["emailAddress"].(map[string]any)
	if emailAddress["address"] != "test@example.com" {
		t.Errorf("expected to address 'test@example.com', got %v", emailAddress["address"])
	}
}

func TestCreateEvent(t *testing.T) {
	var sentPayload map[string]any

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/me/events") {
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if err := json.Unmarshal(data, &sentPayload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id": "event123"}`)),
			}, nil
		}),
	}

	adapter := &Adapter{}
	result, err := adapter.createEvent(context.Background(), client, map[string]any{
		"subject":   "Meeting",
		"start":     "2026-05-07T10:00:00",
		"end":       "2026-05-07T11:00:00",
		"timezone":  "UTC",
		"location":  "Conference Room",
		"attendees": "bob@example.com",
	})
	if err != nil {
		t.Fatalf("createEvent error: %v", err)
	}
	if result == nil {
		t.Fatal("createEvent returned nil result")
	}

	if sentPayload["subject"] != "Meeting" {
		t.Errorf("expected subject 'Meeting', got %v", sentPayload["subject"])
	}
	start, _ := sentPayload["start"].(map[string]any)
	if start["dateTime"] != "2026-05-07T10:00:00" {
		t.Errorf("expected start dateTime '2026-05-07T10:00:00', got %v", start["dateTime"])
	}
}
