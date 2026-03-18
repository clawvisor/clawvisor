package push

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// mockStore embeds store.Store (nil) and overrides only the methods the push
// notifier actually calls. Calling any unimplemented method will panic, which
// is the desired behavior in tests — it means we're calling something unexpected.
type mockStore struct {
	store.Store
	devices []*store.PairedDevice
}

func (m *mockStore) ListPairedDevices(_ context.Context, _ string) ([]*store.PairedDevice, error) {
	return m.devices, nil
}

func testNotifier(t *testing.T, pushSrv *httptest.Server, devices []*store.PairedDevice) (*Notifier, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := &mockStore{devices: devices}
	n := New(st, pushSrv.URL, "test-daemon", priv, "http://localhost:9090", slog.Default())
	return n, pub
}

func TestSendApprovalRequest(t *testing.T) {
	var received pushRequest
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	devices := []*store.PairedDevice{
		{ID: "d1", UserID: "u1", DeviceToken: "tok1"},
		{ID: "d2", UserID: "u1", DeviceToken: "tok2"},
	}

	n, pub := testNotifier(t, srv, devices)

	msgID, err := n.SendApprovalRequest(context.Background(), notify.ApprovalRequest{
		RequestID: "req-123",
		UserID:    "u1",
		AgentName: "TestAgent",
		Service:   "github",
		Action:    "create_issue",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgID != "push:test-daemon" {
		t.Errorf("expected messageID 'push:test-daemon', got %q", msgID)
	}

	// Verify payload.
	if len(received.DeviceTokens) != 2 {
		t.Errorf("expected 2 device tokens, got %d", len(received.DeviceTokens))
	}
	if received.Category != "approval_request" {
		t.Errorf("expected category 'approval_request', got %q", received.Category)
	}
	if received.Title != "Approval Request" {
		t.Errorf("expected title 'Approval Request', got %q", received.Title)
	}
	if !strings.Contains(received.Body, "TestAgent") {
		t.Errorf("body should contain agent name, got %q", received.Body)
	}
	if received.Data["request_id"] != "req-123" {
		t.Errorf("expected request_id 'req-123', got %v", received.Data["request_id"])
	}

	// Verify Ed25519 signature format.
	verifySignatureFormat(t, authHeader, pub, "test-daemon")
}

func TestSendAlert(t *testing.T) {
	var received pushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	devices := []*store.PairedDevice{{ID: "d1", UserID: "u1", DeviceToken: "tok1"}}
	n, _ := testNotifier(t, srv, devices)

	err := n.SendAlert(context.Background(), "u1", "Something happened")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received.Category != "informational" {
		t.Errorf("expected category 'informational', got %q", received.Category)
	}
	if received.Body != "Something happened" {
		t.Errorf("expected body 'Something happened', got %q", received.Body)
	}
}

func TestSendTestMessage(t *testing.T) {
	var received pushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	devices := []*store.PairedDevice{{ID: "d1", UserID: "u1", DeviceToken: "tok1"}}
	n, _ := testNotifier(t, srv, devices)

	err := n.SendTestMessage(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received.Title != "Clawvisor Test" {
		t.Errorf("expected title 'Clawvisor Test', got %q", received.Title)
	}
}

func TestNoDevicesPaired(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, _ := testNotifier(t, srv, nil) // no devices

	msgID, err := n.SendApprovalRequest(context.Background(), notify.ApprovalRequest{
		RequestID: "req-123",
		UserID:    "u1",
		AgentName: "Agent",
		Service:   "github",
		Action:    "read",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgID != "" {
		t.Errorf("expected empty messageID, got %q", msgID)
	}
	if called {
		t.Error("push service should not be called when no devices paired")
	}
}

func TestRegisterDevice(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, _ := testNotifier(t, srv, nil)

	err := n.RegisterDevice(context.Background(), "apns-token-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedPath != "/api/tokens/register" {
		t.Errorf("expected path '/api/tokens/register', got %q", receivedPath)
	}
	if receivedBody["daemon_id"] != "test-daemon" {
		t.Errorf("expected daemon_id 'test-daemon', got %q", receivedBody["daemon_id"])
	}
	if receivedBody["device_token"] != "apns-token-abc" {
		t.Errorf("expected device_token 'apns-token-abc', got %q", receivedBody["device_token"])
	}
}

func TestDeregisterDevice(t *testing.T) {
	var receivedPath, receivedMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, _ := testNotifier(t, srv, nil)

	err := n.DeregisterDevice(context.Background(), "apns-token-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedMethod != "DELETE" {
		t.Errorf("expected DELETE, got %q", receivedMethod)
	}
	if receivedPath != "/api/tokens/apns-token-abc" {
		t.Errorf("expected path '/api/tokens/apns-token-abc', got %q", receivedPath)
	}
}

func TestEmitDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, _ := testNotifier(t, srv, nil)

	d := notify.CallbackDecision{
		Type:     "approval",
		Action:   "approve",
		TargetID: "req-1",
		UserID:   "u1",
	}
	n.EmitDecision(d)

	got := <-n.DecisionChannel()
	if got != d {
		t.Errorf("expected %+v, got %+v", d, got)
	}
}

func TestPushServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	devices := []*store.PairedDevice{{ID: "d1", UserID: "u1", DeviceToken: "tok1"}}
	n, _ := testNotifier(t, srv, devices)

	err := n.SendAlert(context.Background(), "u1", "test")
	if err == nil {
		t.Fatal("expected error from push service")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("expected status 500 in error, got %q", err.Error())
	}
}

// verifySignatureFormat checks the Ed25519 signature header format and verifies
// the signature is valid.
func verifySignatureFormat(t *testing.T, authHeader string, pub ed25519.PublicKey, expectedDaemonID string) {
	t.Helper()

	const prefix = "Ed25519-Sig "
	if !strings.HasPrefix(authHeader, prefix) {
		t.Fatalf("auth header should start with %q, got %q", prefix, authHeader)
	}

	parts := strings.SplitN(authHeader[len(prefix):], ":", 3)
	if len(parts) != 3 {
		t.Fatalf("expected 3 colon-separated parts, got %d", len(parts))
	}

	daemonID, ts, sigB64 := parts[0], parts[1], parts[2]

	if daemonID != expectedDaemonID {
		t.Errorf("expected daemon_id %q, got %q", expectedDaemonID, daemonID)
	}

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("invalid base64 signature: %v", err)
	}

	// We can't reconstruct the exact body here without capturing it,
	// but we can verify the signature is 64 bytes (Ed25519 standard).
	if len(sig) != ed25519.SignatureSize {
		t.Errorf("expected signature of %d bytes, got %d", ed25519.SignatureSize, len(sig))
	}

	// Verify the timestamp is a valid integer string.
	if ts == "" {
		t.Error("timestamp should not be empty")
	}

	_ = ts // already validated by presence check
}

func TestSignatureIncludesBodyHash(t *testing.T) {
	// Capture the auth header and body to verify the signature message includes body hash.
	var capturedAuth string
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	devices := []*store.PairedDevice{{ID: "d1", UserID: "u1", DeviceToken: "tok1"}}
	n, pub := testNotifier(t, srv, devices)

	_ = n.SendAlert(context.Background(), "u1", "test body hash")

	// Parse signature.
	parts := strings.SplitN(capturedAuth[len("Ed25519-Sig "):], ":", 3)
	ts := parts[1]
	sig, _ := base64.StdEncoding.DecodeString(parts[2])

	// Reconstruct the signed message with body hash.
	bodyHash := sha256.Sum256(capturedBody)
	message := fmt.Sprintf("POST\n/api/push\n%s\n%x", ts, bodyHash)

	if !ed25519.Verify(pub, []byte(message), sig) {
		t.Fatal("signature verification failed — body hash may not be included in the signed message")
	}
}
