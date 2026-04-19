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

// GenerateBridgeToken creates a cryptographically secure bridge bearer token
// with the "cvisbr_" prefix. A bridge token authenticates an OpenClaw plugin
// install to Clawvisor — it is intentionally distinct from an agent token so
// the agent (untrusted LLM) cannot forge or observe it. The raw token is
// shown once during pairing; only its SHA-256 hash is stored.
func GenerateBridgeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cvisbr_" + hex.EncodeToString(b), nil
}

// GeneratePluginPairCode creates a one-time capability token used by the
// OpenClaw plugin to initiate a pair request. Shorter than a bearer token
// (18 hex chars = 72 bits) because its risk window is bounded by a short
// expiry (10 min) and single-use consumption. Only the hash is stored
// server-side; the raw value is shown once in the dashboard.
func GeneratePluginPairCode() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cvpc_" + hex.EncodeToString(b), nil
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
