package teams

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
			expectedPath := "/teams/team123/channels/channel456/messages"
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, expectedPath) {
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
				Body:       io.NopCloser(strings.NewReader(`{"id": "msg789"}`)),
			}, nil
		}),
	}

	adapter := &Adapter{}
	result, err := adapter.sendMessage(context.Background(), client, map[string]any{
		"team_id":    "team123",
		"channel_id": "channel456",
		"content":    "<p>Hello Teams</p>",
	})
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if result == nil {
		t.Fatal("sendMessage returned nil result")
	}

	body, ok := sentPayload["body"].(map[string]any)
	if !ok {
		t.Fatalf("missing body object in payload")
	}
	if body["contentType"] != "html" {
		t.Errorf("expected contentType 'html', got %v", body["contentType"])
	}
	if body["content"] != "<p>Hello Teams</p>" {
		t.Errorf("expected content '<p>Hello Teams</p>', got %v", body["content"])
	}
	if result.Data.(map[string]any)["id"] != "msg789" {
		t.Errorf("expected id 'msg789', got %v", result.Data.(map[string]any)["id"])
	}
}
