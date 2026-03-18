package middleware

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/relay"
)

func TestE2E_PlaintextLocal(t *testing.T) {
	// Local requests without E2E headers should pass through.
	daemonKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	handler := E2E(daemonKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))

	req := httptest.NewRequest("POST", "/api/gateway/request", bytes.NewReader([]byte(`{"test":true}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"test":true}` {
		t.Errorf("body: got %q, want {\"test\":true}", rec.Body.String())
	}
}

func TestE2E_RejectPlaintextViaRelay(t *testing.T) {
	// Relay requests without E2E should be rejected with 403.
	daemonKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	handler := E2E(daemonKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/api/gateway/request", nil)
	// Simulate relay context.
	ctx := relay.WithViaRelay(req.Context())
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
}

func TestE2E_EncryptDecryptRoundTrip(t *testing.T) {
	// Full round-trip: encrypt as agent, middleware decrypts, handler sees plaintext,
	// middleware encrypts response, agent decrypts response.
	daemonKey, _ := ecdh.X25519().GenerateKey(rand.Reader)

	plaintext := `{"service":"google.gmail","action":"send_message"}`

	handler := E2E(daemonKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if string(body) != plaintext {
			t.Errorf("decrypted body: got %q, want %q", string(body), plaintext)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"ok"}`))
	}))

	// Agent side: encrypt the request.
	agentKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	shared, _ := agentKey.ECDH(daemonKey.PublicKey())

	block, _ := aes.NewCipher(shared)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	rand.Read(nonce)
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	ciphertextB64 := base64.StdEncoding.EncodeToString(sealed)

	req := httptest.NewRequest("POST", "/api/gateway/request",
		bytes.NewReader([]byte(ciphertextB64)))
	req.Header.Set("X-Clawvisor-E2E", "aes-256-gcm")
	req.Header.Set("X-Clawvisor-Ephemeral-Key",
		base64.StdEncoding.EncodeToString(agentKey.PublicKey().Bytes()))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Verify response is E2E encrypted.
	if rec.Header().Get("X-Clawvisor-E2E") != "aes-256-gcm" {
		t.Fatal("response should have X-Clawvisor-E2E header")
	}

	// Decrypt response.
	respCiphertext, _ := base64.StdEncoding.DecodeString(rec.Body.String())
	if len(respCiphertext) < 12+16 {
		t.Fatal("response ciphertext too short")
	}

	respNonce := respCiphertext[:12]
	respEncData := respCiphertext[12:]
	respPlaintext, err := gcm.Open(nil, respNonce, respEncData, nil)
	if err != nil {
		t.Fatalf("decrypting response: %v", err)
	}

	if string(respPlaintext) != `{"result":"ok"}` {
		t.Errorf("response: got %q, want {\"result\":\"ok\"}", string(respPlaintext))
	}
}

func TestE2E_GETStatusViaRelay(t *testing.T) {
	// GET status requests through relay should work: no body decryption,
	// but response body is encrypted.
	daemonKey, _ := ecdh.X25519().GenerateKey(rand.Reader)

	handler := E2E(daemonKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"completed"}`))
	}))

	// Agent generates ephemeral key for response decryption.
	agentKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	shared, _ := agentKey.ECDH(daemonKey.PublicKey())

	req := httptest.NewRequest("GET", "/api/gateway/request/abc/status", nil)
	ctx := relay.WithViaRelay(req.Context())
	req = req.WithContext(ctx)
	req.Header.Set("X-Clawvisor-E2E", "aes-256-gcm")
	req.Header.Set("X-Clawvisor-Ephemeral-Key",
		base64.StdEncoding.EncodeToString(agentKey.PublicKey().Bytes()))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	// Verify response is encrypted.
	if rec.Header().Get("X-Clawvisor-E2E") != "aes-256-gcm" {
		t.Fatal("response should have X-Clawvisor-E2E header")
	}

	// Decrypt response.
	respCiphertext, _ := base64.StdEncoding.DecodeString(rec.Body.String())
	block, _ := aes.NewCipher(shared)
	gcm, _ := cipher.NewGCM(block)
	respPlaintext, err := gcm.Open(nil, respCiphertext[:12], respCiphertext[12:], nil)
	if err != nil {
		t.Fatalf("decrypting response: %v", err)
	}
	if string(respPlaintext) != `{"status":"completed"}` {
		t.Errorf("response: got %q, want {\"status\":\"completed\"}", string(respPlaintext))
	}
}

func TestE2E_MissingEphemeralKey(t *testing.T) {
	daemonKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	handler := E2E(daemonKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/api/gateway/request", bytes.NewReader([]byte("test")))
	req.Header.Set("X-Clawvisor-E2E", "aes-256-gcm")
	// No X-Clawvisor-Ephemeral-Key header.

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}
