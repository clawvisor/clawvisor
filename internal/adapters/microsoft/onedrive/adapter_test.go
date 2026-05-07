package onedrive

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
		Action:     "list_files",
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

func TestUploadFile(t *testing.T) {
	var reqBody []byte

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPut || !strings.HasSuffix(req.URL.Path, ":/content") {
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			}
			if !strings.Contains(req.URL.Path, "test_file.txt") {
				t.Errorf("expected path to contain test_file.txt, got %s", req.URL.Path)
			}
			var err error
			reqBody, err = io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id": "file123", "name": "test_file.txt", "size": 14}`)),
			}, nil
		}),
	}

	adapter := &Adapter{}
	result, err := adapter.uploadFile(context.Background(), client, map[string]any{
		"path":    "test_file.txt",
		"content": "Hello OneDrive",
	})
	if err != nil {
		t.Fatalf("uploadFile error: %v", err)
	}
	if result == nil {
		t.Fatal("uploadFile returned nil result")
	}

	if string(reqBody) != "Hello OneDrive" {
		t.Errorf("expected body 'Hello OneDrive', got %q", string(reqBody))
	}
	if result.Data.(map[string]any)["id"] != "file123" {
		t.Errorf("expected id 'file123', got %v", result.Data.(map[string]any)["id"])
	}
}

func TestListFiles(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected request method: %s", req.Method)
			}
			if !strings.HasSuffix(req.URL.Path, "children") {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{
					"value": [
						{"id": "folder1", "name": "My Folder", "folder": {}, "size": 0},
						{"id": "file1", "name": "file.txt", "file": {}, "size": 100}
					]
				}`)),
			}, nil
		}),
	}

	adapter := &Adapter{}
	result, err := adapter.listFiles(context.Background(), client, map[string]any{
		"folder_path": "",
	})
	if err != nil {
		t.Fatalf("listFiles error: %v", err)
	}
	if result == nil {
		t.Fatal("listFiles returned nil result")
	}

	items := result.Data.([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0]["type"] != "folder" || items[0]["name"] != "My Folder" {
		t.Errorf("expected first item to be 'My Folder' of type folder, got %v", items[0])
	}
	if items[1]["type"] != "file" || items[1]["name"] != "file.txt" {
		t.Errorf("expected second item to be 'file.txt' of type file, got %v", items[1])
	}
}
