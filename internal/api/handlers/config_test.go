package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigPublicIncludesProxyLitePublicURL(t *testing.T) {
	handler := NewConfigHandler("password", "https://llm.example.com/")
	req := httptest.NewRequest(http.MethodGet, "/api/config/public", nil)
	rec := httptest.NewRecorder()

	handler.Public(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		AuthMode           string `json:"auth_mode"`
		ProxyLitePublicURL string `json:"proxy_lite_public_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if body.AuthMode != "password" {
		t.Fatalf("AuthMode=%q", body.AuthMode)
	}
	if body.ProxyLitePublicURL != "https://llm.example.com" {
		t.Fatalf("ProxyLitePublicURL=%q", body.ProxyLitePublicURL)
	}
}
