package handlers

import (
	"bytes"
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

	// Generate a code.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	var genResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &genResp)
	code := genResp["code"].(string)

	// Verify with the correct code.
	body, _ := json.Marshal(map[string]string{"code": code})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != true {
		t.Error("expected valid: true")
	}
}

func TestPairingVerifyInvalidCode(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// Generate a code.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	// Verify with the wrong code.
	body, _ := json.Marshal(map[string]string{"code": "000000"})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != false {
		t.Error("expected valid: false")
	}
}

func TestPairingVerifyNoActiveCode(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// No code generated — verify should return false.
	body, _ := json.Marshal(map[string]string{"code": "123456"})
	req := httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.VerifyCode(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != false {
		t.Error("expected valid: false when no code active")
	}
}

func TestPairingVerifySingleUse(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// Generate a code.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	var genResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &genResp)
	code := genResp["code"].(string)

	// First verify — should succeed.
	body, _ := json.Marshal(map[string]string{"code": code})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != true {
		t.Fatal("first verify should succeed")
	}

	// Second verify with same code — should fail (consumed).
	body, _ = json.Marshal(map[string]string{"code": code})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != false {
		t.Error("second verify should fail — code was consumed")
	}
}

func TestPairingVerifyExpired(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// Generate code then manually expire it.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	var genResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &genResp)
	code := genResp["code"].(string)

	h.mu.Lock()
	h.created = time.Now().Add(-6 * time.Minute)
	h.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"code": code})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["valid"] != false {
		t.Error("expired code should return valid: false")
	}
}

func TestPairingVerifyBruteForce(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// Generate a code.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	// Send 3 wrong codes.
	for i := 0; i < maxPairingCodeAttempts; i++ {
		body, _ := json.Marshal(map[string]string{"code": "000000"})
		req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		h.VerifyCode(w, req)

		var resp map[string]any
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["valid"] != false {
			t.Errorf("attempt %d: expected valid: false", i+1)
		}
	}

	// Code should be burned — even the correct code should fail.
	h.mu.Lock()
	// The code should have been cleared.
	if h.code != "" {
		t.Error("code should be cleared after max attempts")
	}
	h.mu.Unlock()
}

func TestPairingNewCodeInvalidatesPrevious(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// Generate first code.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	var resp1 map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp1)
	code1 := resp1["code"].(string)

	// Generate second code.
	req = httptest.NewRequest("GET", "/api/pairing/code", nil)
	w = httptest.NewRecorder()
	h.GenerateCode(w, req)

	var resp2 map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp2)
	code2 := resp2["code"].(string)

	// First code should be invalid.
	body, _ := json.Marshal(map[string]string{"code": code1})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	var verifyResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &verifyResp)
	if verifyResp["valid"] != false {
		t.Error("first code should be invalid after generating a new one")
	}

	// Second code should work.
	body, _ = json.Marshal(map[string]string{"code": code2})
	req = httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.VerifyCode(w, req)

	json.Unmarshal(w.Body.Bytes(), &verifyResp)
	if verifyResp["valid"] != true {
		t.Error("second code should be valid")
	}
}

func TestPairingVerifyEmptyCode(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	body, _ := json.Marshal(map[string]string{"code": ""})
	req := httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.VerifyCode(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty code, got %d", w.Code)
	}
}

func TestPairingConcurrentVerify(t *testing.T) {
	h := NewPairingHandler("test-daemon-id")

	// Generate a code.
	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	w := httptest.NewRecorder()
	h.GenerateCode(w, req)

	var genResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &genResp)
	code := genResp["code"].(string)

	// Fire 10 concurrent verify requests with the correct code.
	// Exactly one should succeed.
	const n = 10
	results := make([]bool, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"code": code})
			r := httptest.NewRequest("POST", "/api/pairing/verify", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.VerifyCode(rec, r)

			var resp map[string]any
			json.Unmarshal(rec.Body.Bytes(), &resp)
			results[idx] = resp["valid"] == true
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
