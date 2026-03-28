package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// devicesTestStore is a minimal in-memory store for device handler tests.
type devicesTestStore struct {
	store.Store // embed nil interface; only override needed methods
	devices     map[string]*store.PairedDevice
}

func newDevicesTestStore() *devicesTestStore {
	return &devicesTestStore{devices: make(map[string]*store.PairedDevice)}
}

var devicesTestCounter int

func (s *devicesTestStore) CreatePairedDevice(_ context.Context, d *store.PairedDevice) error {
	if d.ID == "" {
		devicesTestCounter++
		d.ID = fmt.Sprintf("dev-%d", devicesTestCounter)
	}
	d.PairedAt = time.Now()
	d.LastSeenAt = time.Now()
	s.devices[d.ID] = d
	return nil
}

func (s *devicesTestStore) GetPairedDevice(_ context.Context, id string) (*store.PairedDevice, error) {
	d, ok := s.devices[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return d, nil
}

func (s *devicesTestStore) ListPairedDevices(_ context.Context, userID string) ([]*store.PairedDevice, error) {
	var result []*store.PairedDevice
	for _, d := range s.devices {
		if d.UserID == userID {
			result = append(result, d)
		}
	}
	return result, nil
}

func (s *devicesTestStore) ListPairedDevicesByDeviceToken(_ context.Context, deviceToken string) ([]*store.PairedDevice, error) {
	var result []*store.PairedDevice
	for _, d := range s.devices {
		if d.DeviceToken == deviceToken {
			result = append(result, d)
		}
	}
	return result, nil
}

func (s *devicesTestStore) DeletePairedDevice(_ context.Context, id string) error {
	delete(s.devices, id)
	return nil
}

func (s *devicesTestStore) UpdatePairedDeviceLastSeen(_ context.Context, id string) error {
	if d, ok := s.devices[id]; ok {
		d.LastSeenAt = time.Now()
	}
	return nil
}

func newTestDevicesHandler() *DevicesHandler {
	return NewDevicesHandler(newDevicesTestStore(), nil, events.NewHub(), slog.Default(), "http://localhost:9090", nil)
}

func withUser(ctx context.Context, user *store.User) context.Context {
	return context.WithValue(ctx, middleware.UserContextKey, user)
}

func withDevice(ctx context.Context, device *store.PairedDevice) context.Context {
	return context.WithValue(ctx, middleware.DeviceContextKey, device)
}

func TestStartPairing(t *testing.T) {
	h := newTestDevicesHandler()

	req := httptest.NewRequest("POST", "/api/devices/pair", nil)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w := httptest.NewRecorder()

	h.StartPairing(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["pairing_token"] == nil || resp["pairing_token"] == "" {
		t.Error("expected pairing_token in response")
	}
	if resp["code"] == nil || resp["code"] == "" {
		t.Error("expected code in response")
	}
	code := resp["code"].(string)
	if len(code) != 6 {
		t.Errorf("expected 6-digit code, got %q", code)
	}
}

func TestStartPairingUnauthenticated(t *testing.T) {
	h := newTestDevicesHandler()

	req := httptest.NewRequest("POST", "/api/devices/pair", nil)
	w := httptest.NewRecorder()

	h.StartPairing(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestFullPairingFlow(t *testing.T) {
	h := newTestDevicesHandler()

	// Step 1: Start pairing.
	req := httptest.NewRequest("POST", "/api/devices/pair", nil)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w := httptest.NewRecorder()
	h.StartPairing(w, req)

	var startResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &startResp)
	token := startResp["pairing_token"].(string)
	code := startResp["code"].(string)

	// Step 2: Complete pairing.
	completeBody, _ := json.Marshal(map[string]string{
		"pairing_token": token,
		"code":          code,
		"device_name":   "iPhone 15",
		"device_token":  "apns-token-123",
	})
	req = httptest.NewRequest("POST", "/api/devices/pair/complete", bytes.NewReader(completeBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.CompletePairing(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var completeResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &completeResp)
	deviceID := completeResp["device_id"].(string)
	hmacKey := completeResp["hmac_key"].(string)

	if deviceID == "" {
		t.Error("expected device_id in response")
	}
	if hmacKey == "" {
		t.Error("expected hmac_key in response")
	}
	if len(hmacKey) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("expected 64-char hex hmac_key, got %d chars", len(hmacKey))
	}

	// Step 3: List devices.
	req = httptest.NewRequest("GET", "/api/devices", nil)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w = httptest.NewRecorder()
	h.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var devices []*store.PairedDevice
	json.Unmarshal(w.Body.Bytes(), &devices)
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].ID != deviceID {
		t.Errorf("expected device ID %q, got %q", deviceID, devices[0].ID)
	}

	// Step 4: Delete device.
	req = httptest.NewRequest("DELETE", "/api/devices/"+deviceID, nil)
	req.SetPathValue("id", deviceID)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w = httptest.NewRecorder()
	h.Delete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify device is gone.
	req = httptest.NewRequest("GET", "/api/devices", nil)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w = httptest.NewRecorder()
	h.List(w, req)

	json.Unmarshal(w.Body.Bytes(), &devices)
	if len(devices) != 0 {
		t.Errorf("expected 0 devices after delete, got %d", len(devices))
	}
}

func TestBruteForceProtection(t *testing.T) {
	h := newTestDevicesHandler()

	// Start pairing.
	req := httptest.NewRequest("POST", "/api/devices/pair", nil)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w := httptest.NewRecorder()
	h.StartPairing(w, req)

	var startResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &startResp)
	token := startResp["pairing_token"].(string)

	// Send 3 wrong codes.
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(map[string]string{
			"pairing_token": token,
			"code":          "000000", // wrong code
			"device_name":   "iPhone",
			"device_token":  "tok",
		})
		req = httptest.NewRequest("POST", "/api/devices/pair/complete", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		h.CompletePairing(w, req)

		if i < 2 {
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: expected 401, got %d", i+1, w.Code)
			}
			var errResp map[string]any
			json.Unmarshal(w.Body.Bytes(), &errResp)
			if errResp["code"] != "INVALID_CODE" {
				t.Errorf("attempt %d: expected INVALID_CODE, got %v", i+1, errResp["code"])
			}
		} else {
			// 3rd attempt: should be invalidated.
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: expected 401, got %d", i+1, w.Code)
			}
			var errResp map[string]any
			json.Unmarshal(w.Body.Bytes(), &errResp)
			if errResp["code"] != "MAX_ATTEMPTS" {
				t.Errorf("attempt %d: expected MAX_ATTEMPTS, got %v", i+1, errResp["code"])
			}
		}
	}

	// Token should be deleted — any subsequent attempt should return NOT_FOUND.
	body, _ := json.Marshal(map[string]string{
		"pairing_token": token,
		"code":          "000000",
		"device_name":   "iPhone",
		"device_token":  "tok",
	})
	req = httptest.NewRequest("POST", "/api/devices/pair/complete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.CompletePairing(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after max attempts, got %d", w.Code)
	}
}

func TestExpiredPairingSession(t *testing.T) {
	h := newTestDevicesHandler()

	// Manually insert an expired session.
	token := "expired-token"
	h.pairingsMu.Lock()
	h.pairings[token] = &pairingSession{
		Token:     token,
		UserID:    "u1",
		Code:      "123456",
		ExpiresAt: time.Now().Add(-1 * time.Minute), // already expired
	}
	h.pairingsMu.Unlock()

	body, _ := json.Marshal(map[string]string{
		"pairing_token": token,
		"code":          "123456",
		"device_name":   "iPhone",
		"device_token":  "tok",
	})
	req := httptest.NewRequest("POST", "/api/devices/pair/complete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.CompletePairing(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCompletePairingMissingFields(t *testing.T) {
	h := newTestDevicesHandler()

	body, _ := json.Marshal(map[string]string{
		"pairing_token": "tok",
		// missing code, device_name, device_token
	})
	req := httptest.NewRequest("POST", "/api/devices/pair/complete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.CompletePairing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestActionEndpointNoPush(t *testing.T) {
	st := newDevicesTestStore()
	st.devices["d1"] = &store.PairedDevice{
		ID:     "d1",
		UserID: "u1",
	}
	// pushN is nil — action should return 503.
	h := NewDevicesHandler(st, nil, events.NewHub(), slog.Default(), "http://localhost:9090", nil)

	body, _ := json.Marshal(map[string]string{
		"action":       "approve",
		"target_id":    "req-1",
		"request_type": "approval",
	})
	req := httptest.NewRequest("POST", "/api/devices/d1/action", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withDevice(req.Context(), st.devices["d1"]))
	w := httptest.NewRecorder()
	h.Action(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when push not configured, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActionInvalidAction(t *testing.T) {
	h := newTestDevicesHandler()

	body, _ := json.Marshal(map[string]string{
		"action":       "invalid",
		"target_id":    "req-1",
		"request_type": "approval",
	})
	req := httptest.NewRequest("POST", "/api/devices/d1/action", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withDevice(req.Context(), &store.PairedDevice{ID: "d1", UserID: "u1"}))
	w := httptest.NewRecorder()
	h.Action(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDeleteForbidden(t *testing.T) {
	st := newDevicesTestStore()
	st.devices["d1"] = &store.PairedDevice{
		ID:     "d1",
		UserID: "u1",
	}
	h := NewDevicesHandler(st, nil, events.NewHub(), slog.Default(), "http://localhost:9090", nil)

	req := httptest.NewRequest("DELETE", "/api/devices/d1", nil)
	req.SetPathValue("id", "d1")
	// Different user trying to delete.
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u2"}))
	w := httptest.NewRecorder()
	h.Delete(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestCompletePairingDeduplicatesByDeviceToken(t *testing.T) {
	st := newDevicesTestStore()
	h := NewDevicesHandler(st, nil, events.NewHub(), slog.Default(), "http://localhost:9090", nil)

	// Pre-seed a device for a different user with the same device_token.
	st.devices["old-dev"] = &store.PairedDevice{
		ID:          "old-dev",
		UserID:      "other-user",
		DeviceName:  "Old iPhone",
		DeviceToken: "shared-apns-token",
	}

	// Start pairing for user u1.
	req := httptest.NewRequest("POST", "/api/devices/pair", nil)
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w := httptest.NewRecorder()
	h.StartPairing(w, req)

	var startResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &startResp)
	token := startResp["pairing_token"].(string)
	code := startResp["code"].(string)

	// Complete pairing with the same device_token.
	completeBody, _ := json.Marshal(map[string]string{
		"pairing_token": token,
		"code":          code,
		"device_name":   "New iPhone",
		"device_token":  "shared-apns-token",
	})
	req = httptest.NewRequest("POST", "/api/devices/pair/complete", bytes.NewReader(completeBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.CompletePairing(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// The old device should have been removed.
	if _, ok := st.devices["old-dev"]; ok {
		t.Error("expected old device with same device_token to be removed")
	}

	// Only one device should remain (the new one).
	if len(st.devices) != 1 {
		t.Errorf("expected 1 device, got %d", len(st.devices))
	}
}

func TestDeleteNotFound(t *testing.T) {
	h := newTestDevicesHandler()

	req := httptest.NewRequest("DELETE", "/api/devices/nonexistent", nil)
	req.SetPathValue("id", "nonexistent")
	req = req.WithContext(withUser(req.Context(), &store.User{ID: "u1"}))
	w := httptest.NewRecorder()
	h.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
