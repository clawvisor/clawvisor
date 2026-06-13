package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// HashPassword returns a bcrypt hash of the given password.
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(hash), nil
}

// CheckPassword returns nil if password matches the hash, or an error otherwise.
func CheckPassword(password, hash string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// dummyHash is a one-time-computed bcrypt hash used by DummyCheckPassword
// to spend the same wall-clock budget on the user-not-found path as the
// real path. The plaintext is a fresh random 32-byte secret generated at
// package-init time so no real user can collide with it, and the hash is
// never compared against a user-supplied input — only a synthetic value.
var (
	dummyHash     []byte
	dummyHashOnce sync.Once
)

func initDummyHash() {
	dummyHashOnce.Do(func() {
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			// Fall back to a fixed plaintext if crypto/rand fails — a
			// degenerate platform without entropy still gets the timing
			// benefit. The fixed value never lives on disk because the
			// hash is recomputed every process start.
			seed = []byte("clawvisor-dummy-bcrypt-seed-fallback")
		}
		plain := hex.EncodeToString(seed)
		h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
		if err != nil {
			// bcrypt.GenerateFromPassword effectively cannot fail at
			// supported costs; if it does, leave dummyHash nil and
			// DummyCheckPassword falls back to a no-op (slightly faster
			// timing path but still correct semantically).
			return
		}
		dummyHash = h
	})
}

// DummyCheckPassword spends roughly the same wall-clock as CheckPassword
// so callers can use it on user-not-found / disabled-account branches
// without exposing a timing oracle that distinguishes "no such email"
// from "wrong password". The supplied candidate is compared against an
// internal random hash that never matches; the bcrypt cost is the same
// as the production cost. Returns no value — the comparison result is
// intentionally discarded.
func DummyCheckPassword(candidate string) {
	initDummyHash()
	if len(dummyHash) == 0 {
		return
	}
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(candidate))
}
