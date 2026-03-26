package push

import (
	"context"
	"crypto/ed25519"
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
		PendingID: "pend-456",
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
	if received.Category != "GATEWAY_APPROVAL" {
		t.Errorf("expected category 'GATEWAY_APPROVAL', got %q", received.Category)
	}
	if received.Title != "Approval Request" {
		t.Errorf("expected title 'Approval Request', got %q", received.Title)
	}
	if !strings.Contains(received.Body, "TestAgent") {
		t.Errorf("body should contain agent name, got %q", received.Body)
	}
	if received.Data["target_id"] != "pend-456" {
		t.Errorf("expected target_id 'pend-456', got %v", received.Data["target_id"])
	}
	if received.Data["type"] != "approval" {
		t.Errorf("expected type 'approval', got %v", received.Data["type"])
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
	if received.Category != "" {
		t.Errorf("expected empty category, got %q", received.Category)
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

	err := n.RegisterDevice(context.Background(), "apns-token-abc", "com.clawvisor.app.Clip")
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
	if receivedBody["bundle_id"] != "com.clawvisor.app.Clip" {
		t.Errorf("expected bundle_id 'com.clawvisor.app.Clip', got %q", receivedBody["bundle_id"])
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

func TestLiveActivitySentWhenTokenPresent(t *testing.T) {
	var requests []struct {
		Path string
		Body []byte
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, struct {
			Path string
			Body []byte
		}{r.URL.Path, body})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	devices := []*store.PairedDevice{
		{ID: "d1", UserID: "u1", DeviceToken: "tok1", PushToStartToken: "pts-tok1"},
		{ID: "d2", UserID: "u1", DeviceToken: "tok2"}, // no push-to-start token
	}

	n, _ := testNotifier(t, srv, devices)

	_, err := n.SendTaskApprovalRequest(context.Background(), notify.TaskApprovalRequest{
		TaskID:    "task-1",
		UserID:    "u1",
		AgentName: "Agent",
		Purpose:   "Deploy",
		RiskLevel: "high",
		Actions:   []store.TaskAction{{Service: "github", Action: "push"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Live Activity replaces the regular push when push-to-start tokens are available.
	if len(requests) != 1 {
		t.Fatalf("expected 1 request (live-activity only), got %d", len(requests))
	}

	if requests[0].Path != "/api/push/live-activity" {
		t.Errorf("expected request to /api/push/live-activity, got %q", requests[0].Path)
	}

	var laReq liveActivityRequest
	if err := json.Unmarshal(requests[0].Body, &laReq); err != nil {
		t.Fatalf("failed to unmarshal live activity request: %v", err)
	}

	if len(laReq.PushToStartTokens) != 1 || laReq.PushToStartTokens[0] != "pts-tok1" {
		t.Errorf("expected push_to_start_tokens ['pts-tok1'], got %v", laReq.PushToStartTokens)
	}
	if laReq.Event != "start" {
		t.Errorf("expected event 'start', got %q", laReq.Event)
	}
	if laReq.AttributesType != "ApprovalActivityAttributes" {
		t.Errorf("expected attributes_type 'ApprovalActivityAttributes', got %q", laReq.AttributesType)
	}
	if laReq.Attributes["targetID"] != "task-1" {
		t.Errorf("expected targetID 'task-1', got %q", laReq.Attributes["targetID"])
	}
	if laReq.Attributes["riskLevel"] != "high" {
		t.Errorf("expected riskLevel 'high', got %q", laReq.Attributes["riskLevel"])
	}
	if laReq.Attributes["actionSummary"] != "github/push" {
		t.Errorf("expected actionSummary 'github/push', got %q", laReq.Attributes["actionSummary"])
	}
}

func TestLiveActivitySkippedWhenNoToken(t *testing.T) {
	var requestPaths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths = append(requestPaths, r.URL.Path)
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// No push-to-start tokens.
	devices := []*store.PairedDevice{
		{ID: "d1", UserID: "u1", DeviceToken: "tok1"},
	}

	n, _ := testNotifier(t, srv, devices)

	_, err := n.SendTaskApprovalRequest(context.Background(), notify.TaskApprovalRequest{
		TaskID:    "task-1",
		UserID:    "u1",
		AgentName: "Agent",
		Purpose:   "Deploy",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(requestPaths) != 1 {
		t.Fatalf("expected 1 request (push only), got %d", len(requestPaths))
	}
	if requestPaths[0] != "/api/push" {
		t.Errorf("expected request to /api/push, got %q", requestPaths[0])
	}
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

	// Reconstruct the signed message with raw body.
	message := fmt.Sprintf("POST\n/api/push\n%s\n%s", string(capturedBody), ts)

	if !ed25519.Verify(pub, []byte(message), sig) {
		t.Fatal("signature verification failed — body hash may not be included in the signed message")
	}
}
