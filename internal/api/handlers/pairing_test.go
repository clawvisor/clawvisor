package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/relay"
)

func TestPairingGenerateCode(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["daemon_id"] != "test-daemon-id" {
		t.Errorf("expected daemon_id test-daemon-id, got %v", resp["daemon_id"])
	}
	code, ok := resp["code"].(string)
	if !ok || len(code) != 6 {
		t.Errorf("expected 6-digit code, got %q", code)
	}
	if resp["expires_in"] != float64(300) {
		t.Errorf("expected expires_in 300, got %v", resp["expires_in"])
	}
}

func TestPairingGenerateCodeNotConfigured(t *testing.T) {
	h := NewPairingHandler("")

	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestPairingGenerateCodeRejectsRelay(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	ctx := relay.WithViaRelay(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for relay request, got %d", w.Code)
	}
}

func TestPairingVerifyValid(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	code := generateAndGetCode(t, h)

	if !h.Verify(code) {
		t.Error("expected Verify to return true for correct code")
	}
}

func TestPairingVerifyInvalidCode(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	generateAndGetCode(t, h) // generate a code but don't use it

	if h.Verify("000000") {
		t.Error("expected Verify to return false for wrong code")
	}
}

func TestPairingVerifyNoActiveCode(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	if h.Verify("123456") {
		t.Error("expected Verify to return false when no code active")
	}
}

func TestPairingVerifySingleUse(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	code := generateAndGetCode(t, h)

	if !h.Verify(code) {
		t.Fatal("first verify should succeed")
	}
	if h.Verify(code) {
		t.Error("second verify should fail — code was consumed")
	}
}

func TestPairingVerifyExpired(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	// Use a very short expiry to test expiration.
	h.SetPairingCodeStore(newMemoryPairingCodeStore(1*time.Millisecond, maxPairingCodeAttempts))
	code := generateAndGetCode(t, h)

	time.Sleep(5 * time.Millisecond)

	if h.Verify(code) {
		t.Error("expired code should return false")
	}
}

func TestPairingVerifyBruteForce(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	code := generateAndGetCode(t, h)

	for i := 0; i < maxPairingCodeAttempts; i++ {
		if h.Verify("000000") {
			t.Errorf("attempt %d: expected false for wrong code", i+1)
		}
	}

	// After max wrong attempts the code should be invalidated,
	// so even the correct code should fail.
	if h.Verify(code) {
		t.Error("correct code should fail after max brute-force attempts")
	}
}

func TestPairingNewCodeInvalidatesPrevious(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	code1 := generateAndGetCode(t, h)
	code2 := generateAndGetCode(t, h)

	if h.Verify(code1) {
		t.Error("first code should be invalid after generating a new one")
	}

	if !h.Verify(code2) {
		t.Error("second code should be valid")
	}
}

func TestPairingConcurrentVerify(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")
	code := generateAndGetCode(t, h)

	const n = 10
	results := make([]bool, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = h.Verify(code)
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, ok := range results {
		if ok {
			successCount++
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly 1 successful verify, got %d", successCount)
	}
}

// generateAndGetCode calls GenerateCode and returns the 6-digit code string.
func generateAndGetCode(t *testing.T, h *PairingHandler) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GenerateCode failed: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp["code"].(string)
}
