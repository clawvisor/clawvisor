package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// GenerateAgentToken creates a cryptographically secure agent bearer token
// with the "cvis_" prefix. The raw token is shown once to the user; only
// its SHA-256 hash is stored.
func GenerateAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cvis_" + hex.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hex digest of any token string.
// Used for both agent tokens and refresh tokens.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// GenerateCallbackSecret returns a "cbsec_"-prefixed 32-byte hex secret
// used for HMAC-signing callback payloads.
func GenerateCallbackSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cbsec_" + hex.EncodeToString(b), nil
}

// GenerateRandomToken creates a cryptographically secure random hex string
// (no prefix). Used for refresh tokens and other non-agent tokens.
func GenerateRandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
