package haikuproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterRequestShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/register" {
			t.Errorf("expected /v1/register, got %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json Content-Type, got %s", ct)
		}

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body["name"] != "test-project" {
			t.Errorf("expected name=test-project, got %s", body["name"])
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(Registration{
			Key:      "hkp_test123",
			ID:       "id-456",
			Name:     "test-project",
			SpendCap: 1.00,
		})
	}))
	defer srv.Close()

	// Simulate a registration call against the test server.
	resp, err := http.Post(srv.URL+"/v1/register", "application/json",
		strings.NewReader(`{"name":"test-project"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var reg Registration
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if reg.Key != "hkp_test123" {
		t.Errorf("expected key=hkp_test123, got %s", reg.Key)
	}
	if reg.SpendCap != 1.00 {
		t.Errorf("expected spend_cap=1.00, got %f", reg.SpendCap)
	}
}

func TestRegisterRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/register", "application/json",
		strings.NewReader(`{"name":"test"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", resp.StatusCode)
	}
}
