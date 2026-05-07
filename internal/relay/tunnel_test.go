package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
)

func TestViaRelay(t *testing.T) {
	ctx := context.Background()
	if ViaRelay(ctx) {
		t.Error("ViaRelay should be false for plain context")
	}

	ctx = context.WithValue(ctx, viaRelayKey, true)
	if !ViaRelay(ctx) {
		t.Error("ViaRelay should be true when key is set")
	}
}

func TestHandleRequest(t *testing.T) {
	// Set up a handler that echoes back the request info.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify ViaRelay is set.
		if !ViaRelay(r.Context()) {
			t.Error("ViaRelay should be true for relay-dispatched requests")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method,
			"path":   r.URL.Path,
			"auth":   r.Header.Get("Authorization"),
		})
	})

	// Create a minimal client with the handler.
	c := &Client{
		handler: handler,
		logger:  slog.Default(),
	}

	// Set up a channel to capture the response.
	var captured *HTTPResponsePayload
	origSendResponse := c.sendResponse
	_ = origSendResponse // sendResponse is a method, we'll check via a wrapper

	// Build request payload.
	bodyBytes := []byte(`{"test": true}`)
	payload := HTTPRequestPayload{
		Method:  "POST",
		Path:    "/api/gateway/request",
		Headers: map[string][]string{"Authorization": {"Bearer test_token"}},
		Body:    base64.StdEncoding.EncodeToString(bodyBytes),
	}

	// We can't easily capture sendResponse since it writes to WebSocket,
	// so instead we test handleRequest directly by providing a mock conn.
	// For now, test the dispatch path only (handler is called correctly).

	// Create a test that verifies the handler sees the right request.
	handlerCalled := false
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		if r.Method != "POST" {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/gateway/request" {
			t.Errorf("path: got %q, want /api/gateway/request", r.URL.Path)
		}
		if !ViaRelay(r.Context()) {
			t.Error("ViaRelay should be true")
		}
		if r.Header.Get("Authorization") != "Bearer test_token" {
			t.Errorf("auth: got %q, want Bearer test_token", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})

	c2 := &Client{
		handler: testHandler,
		logger:  slog.Default(),
	}
	// handleRequest will try to sendResponse (which needs a conn).
	// We just verify the handler is called without panicking on nil conn.
	c2.handleRequest(context.Background(), "test-id", payload)

	if !handlerCalled {
		t.Error("handler was not called")
	}

	_ = c
	_ = captured
}

// TestHandleRequest_ClientIPSetsRemoteAddr is the regression guard for the
// "relay traffic has empty RemoteAddr" bug. When the relay envelope carries
// a ClientIP, the synthesized request must surface it as r.RemoteAddr so
// per-IP rate limits and audit logs apply correctly.
func TestHandleRequest_ClientIPSetsRemoteAddr(t *testing.T) {
	gotRemoteAddr := ""
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
		w.WriteHeader(http.StatusOK)
	})
	c := &Client{handler: handler, logger: slog.Default()}

	payload := HTTPRequestPayload{
		Method:   "GET",
		Path:     "/api/health",
		Headers:  map[string][]string{},
		Body:     "",
		ClientIP: "203.0.113.7",
	}
	c.handleRequest(context.Background(), "test-id", payload)

	if gotRemoteAddr != "203.0.113.7:0" {
		t.Fatalf("expected RemoteAddr=203.0.113.7:0, got %q", gotRemoteAddr)
	}
}
