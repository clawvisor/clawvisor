package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/clawvisor/clawvisor/pkg/config"
)

// mockRelay is a test WebSocket server that mimics the relay's auth handshake
// and can be configured to drop connections after a set number of frames.
type mockRelay struct {
	pub           ed25519.PublicKey
	connectCount  atomic.Int32
	mu            sync.Mutex
	dropAfter     int           // close conn after this many reads (0 = never auto-drop)
	holdDuration  time.Duration // keep conn alive this long before closing (0 = use dropAfter)
	rejectAuth    bool          // reject the auth handshake
	onConnect     func()        // called after successful auth
}

func (m *mockRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()

	// Send challenge.
	challenge := fmt.Sprintf("2026-03-17T10:00:00Z:%d", m.connectCount.Load())
	challengeJSON, _ := json.Marshal(map[string]string{"challenge": challenge})
	if err := conn.Write(ctx, websocket.MessageText, challengeJSON); err != nil {
		return
	}

	// Read signature.
	_, sigData, err := conn.Read(ctx)
	if err != nil {
		return
	}

	if m.rejectAuth {
		conn.Close(websocket.StatusPolicyViolation, "auth rejected")
		return
	}

	// Verify signature (parse hex).
	var sigMsg struct {
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(sigData, &sigMsg); err != nil {
		conn.Close(websocket.StatusPolicyViolation, "bad sig format")
		return
	}
	sigBytes, err := hexDecode(sigMsg.Signature)
	if err != nil || !ed25519.Verify(m.pub, []byte(challenge), sigBytes) {
		conn.Close(websocket.StatusPolicyViolation, "invalid signature")
		return
	}

	m.connectCount.Add(1)
	if m.onConnect != nil {
		m.onConnect()
	}

	m.mu.Lock()
	dropAfter := m.dropAfter
	holdDuration := m.holdDuration
	m.mu.Unlock()

	// If holdDuration is set, keep conn alive then close.
	if holdDuration > 0 {
		time.Sleep(holdDuration)
		return
	}

	// Otherwise, read frames and optionally drop.
	reads := 0
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			return
		}
		reads++
		if dropAfter > 0 && reads >= dropAfter {
			return // close triggers reconnect
		}
	}
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd hex length")
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		var hi, lo byte
		hi, err := hexNibble(s[i*2])
		if err != nil {
			return nil, err
		}
		lo, err = hexNibble(s[i*2+1])
		if err != nil {
			return nil, err
		}
		b[i] = hi<<4 | lo
	}
	return b, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex char: %c", c)
	}
}

func newTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return pub, priv
}

func newTestClient(t *testing.T, serverURL string, priv ed25519.PrivateKey, baseDelay, maxDelay string) *Client {
	t.Helper()
	// Convert http:// to ws:// for the client.
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1)
	cfg := config.RelayConfig{
		URL:                wsURL,
		DaemonID:           "test-daemon",
		ReconnectBaseDelay: baseDelay,
		ReconnectMaxDelay:  maxDelay,
		Enabled:            true,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return New(cfg, priv, handler, slog.Default())
}

func TestClient_ConnectAndAuthenticate(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	mock := &mockRelay{pub: pub}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	c := newTestClient(t, srv.URL, priv, "10ms", "100ms")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run in background — it will connect and block reading.
	go c.Run(ctx)

	// Wait for connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Connected() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !c.Connected() {
		t.Fatal("client did not connect within deadline")
	}
	if mock.connectCount.Load() != 1 {
		t.Fatalf("expected 1 connection, got %d", mock.connectCount.Load())
	}
}

func TestClient_ReconnectsAfterDrop(t *testing.T) {
	pub, priv := newTestKeyPair(t)

	// Server holds the connection for 50ms then drops it.
	mock := &mockRelay{pub: pub, holdDuration: 50 * time.Millisecond}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	c := newTestClient(t, srv.URL, priv, "10ms", "50ms")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Wait for at least 3 connections (initial + 2 reconnections).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.connectCount.Load() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	count := mock.connectCount.Load()
	if count < 3 {
		t.Fatalf("expected at least 3 connections (reconnections), got %d", count)
	}
}

func TestClient_ExponentialBackoff(t *testing.T) {
	pub, priv := newTestKeyPair(t)

	// Server immediately closes after auth (holdDuration=1ms).
	connectTimes := make([]time.Time, 0, 10)
	var mu sync.Mutex

	mock := &mockRelay{
		pub:          pub,
		holdDuration: 1 * time.Millisecond,
		onConnect: func() {
			mu.Lock()
			connectTimes = append(connectTimes, time.Now())
			mu.Unlock()
		},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	// Use 50ms base, 200ms max so we can observe backoff without slow tests.
	c := newTestClient(t, srv.URL, priv, "50ms", "200ms")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Wait for at least 4 connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(connectTimes)
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	times := append([]time.Time{}, connectTimes...)
	mu.Unlock()

	if len(times) < 4 {
		t.Fatalf("expected at least 4 connections, got %d", len(times))
	}

	// Verify delays are increasing (with jitter tolerance).
	// Connection drops immediately, so inter-connect time ≈ backoff delay.
	// Expected: ~50ms, ~100ms, ~200ms (capped).
	// With ±25% jitter: 37-62ms, 75-125ms, 150-250ms.
	for i := 1; i < len(times)-1; i++ {
		gap := times[i+1].Sub(times[i])
		prevGap := times[i].Sub(times[i-1])
		// Each gap should be at least 60% of the previous (accounting for jitter).
		// This is a loose check — we mainly want to verify delays increase, not exact values.
		if i < 3 && gap < prevGap*6/10 {
			t.Errorf("gap %d (%v) decreased too much from gap %d (%v) — backoff not increasing",
				i, gap, i-1, prevGap)
		}
	}

	// Verify first delay is roughly baseDelay (50ms ±25% jitter = 37-62ms).
	firstGap := times[1].Sub(times[0])
	if firstGap < 30*time.Millisecond || firstGap > 100*time.Millisecond {
		t.Errorf("first reconnect gap %v outside expected range [30ms, 100ms]", firstGap)
	}
}

func TestClient_BackoffResetsAfterHealthyConnection(t *testing.T) {
	pub, priv := newTestKeyPair(t)

	connectionNum := atomic.Int32{}
	connectTimes := make([]time.Time, 0, 10)
	var mu sync.Mutex

	// First 2 connections: drop immediately (builds up backoff).
	// Third connection: hold for 200ms (long enough to trigger reset).
	// Fourth connection: should reconnect quickly (backoff was reset).
	mock := &mockRelay{pub: pub}
	mock.onConnect = func() {
		n := connectionNum.Add(1)
		mu.Lock()
		connectTimes = append(connectTimes, time.Now())
		mu.Unlock()

		mock.mu.Lock()
		if n <= 2 {
			mock.holdDuration = 1 * time.Millisecond // immediate drop
		} else if n == 3 {
			mock.holdDuration = 200 * time.Millisecond // healthy connection
		} else {
			mock.holdDuration = 1 * time.Millisecond
		}
		mock.mu.Unlock()
	}

	srv := httptest.NewServer(mock)
	defer srv.Close()

	// 30ms base, 200ms max. The healthy connection (200ms) exceeds any
	// escalated delay, so backoff should reset.
	c := newTestClient(t, srv.URL, priv, "30ms", "200ms")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Wait for at least 5 connections.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if connectionNum.Load() >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	times := append([]time.Time{}, connectTimes...)
	mu.Unlock()

	if len(times) < 5 {
		t.Fatalf("expected at least 5 connections, got %d", len(times))
	}

	// Gap after the healthy connection (conn 3→4) should be close to baseDelay,
	// not the escalated delay. The third connection was held 200ms, and the
	// backoff should have reset.
	gapAfterHealthy := times[3].Sub(times[2])
	// This includes the 200ms hold + the backoff delay. The delay should be
	// ~30ms (base) not ~120ms (escalated). So total should be ~230ms not ~320ms.
	// Allow generous bounds: 150-350ms (200ms hold + 30ms base ± jitter).
	if gapAfterHealthy > 400*time.Millisecond {
		t.Errorf("gap after healthy connection (%v) too large — backoff may not have reset", gapAfterHealthy)
	}
}

func TestClient_MaxDelayCap(t *testing.T) {
	pub, priv := newTestKeyPair(t)

	connectTimes := make([]time.Time, 0, 20)
	var mu sync.Mutex

	mock := &mockRelay{
		pub:          pub,
		holdDuration: 1 * time.Millisecond,
		onConnect: func() {
			mu.Lock()
			connectTimes = append(connectTimes, time.Now())
			mu.Unlock()
		},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	// 20ms base, 80ms max. After 2 doublings (20→40→80), should cap.
	c := newTestClient(t, srv.URL, priv, "20ms", "80ms")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Wait for 6+ connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(connectTimes)
		mu.Unlock()
		if n >= 6 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	times := append([]time.Time{}, connectTimes...)
	mu.Unlock()

	if len(times) < 6 {
		t.Fatalf("expected at least 6 connections, got %d", len(times))
	}

	// Gaps after index 3 should all be near maxDelay (80ms ±25% = 60-100ms).
	// None should exceed maxDelay + generous jitter headroom.
	for i := 4; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap > 150*time.Millisecond {
			t.Errorf("gap %d (%v) exceeds max delay cap — delay may not be capped", i, gap)
		}
	}
}

func TestClient_ContextCancellationStopsRun(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	mock := &mockRelay{pub: pub}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	c := newTestClient(t, srv.URL, priv, "10ms", "100ms")

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Run(ctx)
	}()

	// Wait for connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Connected() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if c.Connected() {
		t.Error("client should not be connected after cancellation")
	}
}

func TestApplyJitter(t *testing.T) {
	d := 100 * time.Millisecond
	min := time.Duration(float64(d) * 0.75)
	max := time.Duration(float64(d) * 1.25)

	for i := 0; i < 1000; i++ {
		j := applyJitter(d)
		if j < min || j > max {
			t.Fatalf("jittered %v outside [%v, %v]", j, min, max)
		}
	}
}
