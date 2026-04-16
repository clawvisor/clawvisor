package pairing

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"time"
)

const (
	codeLifetime = 60 * time.Second
	codeLength   = 6
	nonceLength  = 12 // 12 hex chars = 6 bytes
)

// CodeManager manages the generation and validation of pairing codes.
type CodeManager struct {
	mu      sync.Mutex
	code    string
	nonce   string
	expires time.Time
}

// NewCodeManager creates a new pairing code manager.
func NewCodeManager() *CodeManager {
	return &CodeManager{}
}

// CurrentCode returns the current pairing code, generating a new one if needed.
func (m *CodeManager) CurrentCode() (code, nonce string, expiresAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.code == "" || time.Now().After(m.expires) {
		m.generate()
	}

	return m.code, m.nonce, m.expires
}

// Validate checks if the given code and nonce match the current active pair.
// On success, the code is consumed (invalidated).
func (m *CodeManager) Validate(code, nonce string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.code == "" || time.Now().After(m.expires) {
		return false
	}
	// Constant-time comparison prevents timing side-channels from narrowing the
	// 6-digit code / 12-hex-char nonce space within the 60s window.
	codeOK := subtle.ConstantTimeCompare([]byte(m.code), []byte(code)) == 1
	nonceOK := subtle.ConstantTimeCompare([]byte(m.nonce), []byte(nonce)) == 1
	if !codeOK || !nonceOK {
		return false
	}

	// Consume the code.
	m.generate()
	return true
}

func (m *CodeManager) generate() {
	m.code = generateNumericCode(codeLength)
	m.nonce = generateNonce(nonceLength)
	m.expires = time.Now().Add(codeLifetime)
}

func generateNumericCode(length int) string {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(length)), nil)
	n, _ := rand.Int(rand.Reader, max)
	return fmt.Sprintf("%0*d", length, n)
}

func generateNonce(hexLength int) string {
	b := make([]byte, hexLength/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
