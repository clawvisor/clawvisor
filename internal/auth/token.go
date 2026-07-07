package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
)

// APITokenPrefix is the canonical prefix for long-lived API tokens
// (spec 05). Distinct from agent tokens (cvis_) so the two auth paths
// stay disjoint.
const APITokenPrefix = "cvat_"

// APITokenPrefixLen is how many leading characters of the plaintext are
// stored as api_tokens.token_prefix for display in lists (e.g.
// "cvat_AbC1dEf2gH").
const APITokenPrefixLen = 16

// apiTokenRE is the exact shape the server accepts and the Terraform
// module (spec 03) generates to: cvat_ + 43 base64url chars (RFC 4648
// §5, no padding) encoding 32 random bytes. base64.RawURLEncoding of 32
// bytes is always 43 chars.
var apiTokenRE = regexp.MustCompile(`^cvat_[A-Za-z0-9_-]{43}$`)

// GenerateAPIToken mints a new API token: "cvat_" + 43 base64url chars.
// It returns the plaintext (shown once) and the display prefix (first 16
// chars) stored in api_tokens.token_prefix. Only the SHA-256 hash
// (HashToken) is persisted. This is base64url, NOT hex — do not confuse
// it with GenerateAgentToken (cvis_ + hex).
func GenerateAPIToken() (token, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = APITokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, token[:APITokenPrefixLen], nil
}

// ValidAPITokenFormat reports whether s matches the canonical API-token
// shape. The server refuses to start on / rejects any bootstrap or
// presented token that does not match.
func ValidAPITokenFormat(s string) bool {
	return apiTokenRE.MatchString(s)
}

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

// InviteTokenPrefix is the canonical prefix for single-use enrollment
// invites (spec 04). Distinct from agent (cvis_) and API (cvat_) tokens so
// the auth paths stay disjoint. An invite is a bearer credential: only the
// SHA-256 hash lives at rest and the plaintext is revealed exactly once.
const InviteTokenPrefix = "cvinv_"

// GenerateInviteToken mints a new invite token: "cvinv_" + 64 hex chars
// (32 random bytes). Follows the GenerateAgentToken pattern; only its
// HashToken digest is persisted.
func GenerateInviteToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return InviteTokenPrefix + hex.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hex digest of any token string.
// Used for both agent tokens and refresh tokens.
func HashToken(token string) string {
	// codeql[go/weak-sensitive-data-hashing] Tokens are high-entropy random values; SHA-256 is used only for internal lookup/deduplication, not password storage.
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
