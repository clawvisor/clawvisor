package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSAllowedOrigin(t *testing.T) {
	handler := CORSAllowOrigins(
		[]string{"https://relay.clawvisor.com"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	req.Header.Set("Origin", "https://relay.clawvisor.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://relay.clawvisor.com" {
		t.Errorf("expected ACAO header, got %q", got)
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	handler := CORSAllowOrigins(
		[]string{"https://relay.clawvisor.com"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no ACAO header for disallowed origin, got %q", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler should not be called for OPTIONS")
	})
	handler := CORSAllowOrigins([]string{"https://relay.clawvisor.com"}, inner)

	req := httptest.NewRequest("OPTIONS", "/api/pairing/code", nil)
	req.Header.Set("Origin", "https://relay.clawvisor.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for preflight, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("expected Access-Control-Allow-Methods header")
	}
}

func TestCORSNoOrigin(t *testing.T) {
	handler := CORSAllowOrigins(
		[]string{"https://relay.clawvisor.com"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/pairing/code", nil)
	// No Origin header — same-origin or non-browser request.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no ACAO header without Origin, got %q", got)
	}
}
