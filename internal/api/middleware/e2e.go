package middleware

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/relay"
	"golang.org/x/crypto/hkdf"
)

// e2eError writes a JSON error response with the correct content type so that
// clients using content-type sniffing can detect and retry E2E failures.
func e2eError(w http.ResponseWriter, body string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write([]byte(body))
	w.Write([]byte("\n"))
}

// E2E wraps a gateway handler to transparently decrypt E2E-encrypted request
// bodies and encrypt response bodies. The gateway handler is unaware of E2E —
// it sees plaintext requests and writes plaintext responses.
func E2E(x25519Key *ecdh.PrivateKey) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viaRelay := relay.ViaRelay(r.Context())

			if r.Header.Get("X-Clawvisor-E2E") == "" {
				if viaRelay {
					e2eError(w, `{"error":"E2E encryption required for requests through relay","code":"E2E_REQUIRED"}`, http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// Read ephemeral public key from header.
			ephKeyB64 := r.Header.Get("X-Clawvisor-Ephemeral-Key")
			if ephKeyB64 == "" {
				e2eError(w, `{"error":"missing X-Clawvisor-Ephemeral-Key header","code":"E2E_ERROR"}`, http.StatusBadRequest)
				return
			}

			ephKeyBytes, err := base64.StdEncoding.DecodeString(ephKeyB64)
			if err != nil {
				e2eError(w, `{"error":"invalid ephemeral key encoding","code":"E2E_ERROR"}`, http.StatusBadRequest)
				return
			}

			ephPub, err := ecdh.X25519().NewPublicKey(ephKeyBytes)
			if err != nil {
				e2eError(w, `{"error":"invalid ephemeral public key","code":"E2E_ERROR"}`, http.StatusBadRequest)
				return
			}

			// ECDH → shared secret → HKDF derived key.
			rawShared, err := x25519Key.ECDH(ephPub)
			if err != nil {
				e2eError(w, `{"error":"ECDH key exchange failed","code":"E2E_ERROR"}`, http.StatusInternalServerError)
				return
			}
			shared := make([]byte, 32)
			if _, err := io.ReadFull(hkdf.New(sha256.New, rawShared, nil, []byte("clawvisor-e2e-v1")), shared); err != nil {
				e2eError(w, `{"error":"key derivation failed","code":"E2E_ERROR"}`, http.StatusInternalServerError)
				return
			}

			// Decrypt request body if present. GET requests (e.g. status
			// polling) have no body — only the response needs encryption.
			if r.Body != nil && r.ContentLength != 0 {
				ciphertextB64, err := io.ReadAll(r.Body)
				if err != nil {
					e2eError(w, `{"error":"failed to read request body","code":"E2E_ERROR"}`, http.StatusBadRequest)
					return
				}
				r.Body.Close()

				if len(ciphertextB64) > 0 {
					ciphertext, err := base64.StdEncoding.DecodeString(string(ciphertextB64))
					if err != nil {
						e2eError(w, `{"error":"invalid ciphertext encoding","code":"E2E_ERROR"}`, http.StatusBadRequest)
						return
					}

					if len(ciphertext) < 12+16 {
						e2eError(w, `{"error":"ciphertext too short","code":"E2E_ERROR"}`, http.StatusBadRequest)
						return
					}

					nonce := ciphertext[:12]
					encData := ciphertext[12:]

					block, err := aes.NewCipher(shared)
					if err != nil {
						e2eError(w, `{"error":"cipher init failed","code":"E2E_ERROR"}`, http.StatusInternalServerError)
						return
					}
					gcm, err := cipher.NewGCM(block)
					if err != nil {
						e2eError(w, `{"error":"GCM init failed","code":"E2E_ERROR"}`, http.StatusInternalServerError)
						return
					}

					plaintext, err := gcm.Open(nil, nonce, encData, nil)
					if err != nil {
						e2eError(w, `{"error":"decryption failed","code":"E2E_ERROR"}`, http.StatusBadRequest)
						return
					}

					r.Body = io.NopCloser(bytes.NewReader(plaintext))
					r.ContentLength = int64(len(plaintext))
				}
			}

			// Wrap ResponseWriter to encrypt the response body.
			ew := &e2eWriter{
				ResponseWriter: w,
				shared:         shared,
			}

			next.ServeHTTP(ew, r)

			// Flush encrypted response.
			ew.flush()
		})
	}
}

// e2eWriter captures the response body and encrypts it before sending.
type e2eWriter struct {
	http.ResponseWriter
	shared      []byte
	buf         bytes.Buffer
	wroteHeader bool
	statusCode  int
}

func (ew *e2eWriter) WriteHeader(code int) {
	ew.statusCode = code
	ew.wroteHeader = true
}

func (ew *e2eWriter) Write(b []byte) (int, error) {
	if !ew.wroteHeader {
		ew.statusCode = http.StatusOK
		ew.wroteHeader = true
	}
	return ew.buf.Write(b)
}

func (ew *e2eWriter) flush() {
	plaintext := ew.buf.Bytes()

	block, err := aes.NewCipher(ew.shared)
	if err != nil {
		ew.ResponseWriter.WriteHeader(http.StatusInternalServerError)
		return
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		ew.ResponseWriter.WriteHeader(http.StatusInternalServerError)
		return
	}

	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		ew.ResponseWriter.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Encrypt: nonce || ciphertext || tag (GCM appends tag to ciphertext).
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)

	encoded := base64.StdEncoding.EncodeToString(sealed)

	ew.ResponseWriter.Header().Set("X-Clawvisor-E2E", "aes-256-gcm")
	ew.ResponseWriter.WriteHeader(ew.statusCode)
	ew.ResponseWriter.Write([]byte(encoded))
}
