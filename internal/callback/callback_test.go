package callback

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// resetInit restores package globals after a test mutates them via Init.
func resetInit(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { Init(nil, false, false) })
}

func TestValidateCallbackURL(t *testing.T) {
	resetInit(t)
	Init(nil, true, false)

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://example.com/cb", false},
		{"localhost http allowed", "http://localhost:8080/cb", false},
		{"127.0.0.1 http allowed", "http://127.0.0.1:8080/cb", false},
		{"http external rejected", "http://example.com/cb", true},
		{"unknown scheme rejected", "ftp://example.com/cb", true},
		{"unparseable", "://broken", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCallbackURL(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCallbackURL(%q) err=%v wantErr=%v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestIsSSRFTarget(t *testing.T) {
	resetInit(t)
	Init(nil, false, true)

	blocked := []string{
		"10.0.0.1",
		"172.16.5.5",
		"192.168.1.1",
		"169.254.169.254", // cloud metadata
		"fc00::1",
		"fe80::1",
		"127.0.0.1",
		"::1",
	}
	for _, ipStr := range blocked {
		t.Run("blocked/"+ipStr, func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				t.Fatalf("parse %q failed", ipStr)
			}
			if !isSSRFTarget(ip) {
				t.Fatalf("expected %s to be blocked", ipStr)
			}
		})
	}

	allowed := []string{
		"8.8.8.8",
		"203.0.113.5",
		"2001:db8::1",
	}
	for _, ipStr := range allowed {
		t.Run("allowed/"+ipStr, func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				t.Fatalf("parse %q failed", ipStr)
			}
			if isSSRFTarget(ip) {
				t.Fatalf("expected %s to be allowed", ipStr)
			}
		})
	}
}

func TestIsSSRFTarget_LoopbackBlockedOnlyWhenConfigured(t *testing.T) {
	resetInit(t)
	Init(nil, false, false)
	if isSSRFTarget(net.ParseIP("127.0.0.1")) {
		t.Fatalf("loopback should NOT be blocked when blockLoopback=false")
	}
	Init(nil, false, true)
	if !isSSRFTarget(net.ParseIP("127.0.0.1")) {
		t.Fatalf("loopback SHOULD be blocked when blockLoopback=true")
	}
}

func TestIsSSRFTarget_AllowlistOverridesBlock(t *testing.T) {
	resetInit(t)
	Init([]string{"10.0.0.0/8"}, false, false)
	if isSSRFTarget(net.ParseIP("10.1.2.3")) {
		t.Fatalf("explicitly allowlisted CIDR should not be blocked")
	}
	if !isSSRFTarget(net.ParseIP("192.168.0.1")) {
		t.Fatalf("non-allowlisted private range should remain blocked")
	}
}

func TestDeliverResult_NoURL_NoOp(t *testing.T) {
	resetInit(t)
	if err := DeliverResult(context.Background(), "", &Payload{}, ""); err != nil {
		t.Fatalf("expected nil error for empty callback URL, got %v", err)
	}
}

func TestDeliverResult_HMACSignatureMatchesBody(t *testing.T) {
	resetInit(t)
	Init(nil, false, false)

	const secret = "agent-bearer-token-xyz"

	var seenSig string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSig = r.Header.Get("X-Clawvisor-Signature")
		body, _ := io.ReadAll(r.Body)
		seenBody = body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	payload := &Payload{Type: "request", RequestID: "req-1", Status: "approved"}
	if err := DeliverResult(context.Background(), srv.URL, payload, secret); err != nil {
		t.Fatalf("DeliverResult: %v", err)
	}

	// Recompute the expected HMAC from the body the server actually saw.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(seenBody)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if seenSig != expected {
		t.Fatalf("signature mismatch: got %q want %q", seenSig, expected)
	}

	// Confirm body shape.
	var got Payload
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if got.RequestID != payload.RequestID || got.Status != payload.Status {
		t.Fatalf("payload mismatch: %+v", got)
	}
}

func TestDeliverResult_OmitsSignatureWhenNoSecret(t *testing.T) {
	resetInit(t)
	Init(nil, false, false)

	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig = r.Header.Get("X-Clawvisor-Signature")
	}))
	t.Cleanup(srv.Close)

	if err := DeliverResult(context.Background(), srv.URL, &Payload{Type: "request", RequestID: "r"}, ""); err != nil {
		t.Fatalf("DeliverResult: %v", err)
	}
	if sig != "" {
		t.Fatalf("expected no signature header when secret is empty, got %q", sig)
	}
}

func TestDeliverResult_NonSuccessReturnsError(t *testing.T) {
	resetInit(t)
	Init(nil, false, false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("kaboom"))
	}))
	t.Cleanup(srv.Close)

	err := DeliverResult(context.Background(), srv.URL, &Payload{Type: "request", RequestID: "r"}, "")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestDeliverResult_BlocksSSRFTargets(t *testing.T) {
	resetInit(t)
	// Block loopback so an httptest server (which binds to 127.0.0.1) will
	// be rejected by the SSRF dialer. This proves the dialer enforces blocks
	// even when ValidateCallbackURL accepted the host.
	Init(nil, false, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := DeliverResult(context.Background(), srv.URL, &Payload{Type: "request", RequestID: "r"}, "")
	if err == nil {
		t.Fatalf("expected SSRF block error, got nil")
	}
	if !strings.Contains(err.Error(), "blocked IP") && !strings.Contains(err.Error(), "no safe IPs") {
		t.Fatalf("expected SSRF block error, got %v", err)
	}
}

func TestDeliverResult_ConcurrencySafe(t *testing.T) {
	resetInit(t)
	Init(nil, false, false)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = DeliverResult(context.Background(), srv.URL, &Payload{Type: "request", RequestID: "r"}, "k")
		}()
	}
	wg.Wait()
	if got := hits.Load(); got != n {
		t.Fatalf("expected %d hits, got %d", n, got)
	}
}
