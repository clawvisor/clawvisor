package vault

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	pkgvault "github.com/clawvisor/clawvisor/pkg/vault"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestLocalVault_EncryptDecryptRoundTrip(t *testing.T) {
	v, err := NewLocalVaultFromKey(newKey(t))
	if err != nil {
		t.Fatalf("NewLocalVaultFromKey: %v", err)
	}

	cases := [][]byte{
		[]byte("hello"),
		[]byte(""),
		bytes.Repeat([]byte{0xAB}, 4096),
		[]byte(`{"token":"sk_test_abcdef"}`),
	}
	for _, plaintext := range cases {
		aad := []byte("u|svc")
		enc, iv, tag, err := v.Encrypt(plaintext, aad)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		got, err := v.Decrypt(enc, iv, tag, aad)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("round-trip mismatch: want %q got %q", plaintext, got)
		}
	}
}

func TestLocalVault_EncryptUsesFreshNonce(t *testing.T) {
	v, err := NewLocalVaultFromKey(newKey(t))
	if err != nil {
		t.Fatalf("NewLocalVaultFromKey: %v", err)
	}
	plaintext := []byte("same plaintext, different ciphertexts please")
	aad := []byte("u|svc")
	_, iv1, _, err := v.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	_, iv2, _, err := v.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if iv1 == iv2 {
		t.Fatalf("nonces must differ across encryptions, both = %q", iv1)
	}
}

func TestLocalVault_TamperedCiphertextFails(t *testing.T) {
	v, err := NewLocalVaultFromKey(newKey(t))
	if err != nil {
		t.Fatalf("NewLocalVaultFromKey: %v", err)
	}
	aad := []byte("u|svc")
	enc, iv, tag, err := v.Encrypt([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode enc: %v", err)
	}
	if len(raw) == 0 {
		raw = []byte{0x00}
	}
	raw[0] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := v.Decrypt(tampered, iv, tag, aad); err == nil {
		t.Fatalf("expected decrypt error for tampered ciphertext")
	}
}

func TestLocalVault_TamperedAuthTagFails(t *testing.T) {
	v, err := NewLocalVaultFromKey(newKey(t))
	if err != nil {
		t.Fatalf("NewLocalVaultFromKey: %v", err)
	}
	aad := []byte("u|svc")
	enc, iv, tag, err := v.Encrypt([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(tag)
	if err != nil {
		t.Fatalf("decode tag: %v", err)
	}
	raw[0] ^= 0x01
	bad := base64.StdEncoding.EncodeToString(raw)
	if _, err := v.Decrypt(enc, iv, bad, aad); err == nil {
		t.Fatalf("expected decrypt error for tampered auth tag")
	}
}

func TestLocalVault_WrongKeyFails(t *testing.T) {
	plaintext := []byte("cross-key isolation matters")
	keyA := newKey(t)
	keyB := append([]byte(nil), keyA...)
	keyB[0] ^= 0xFF

	a, err := NewLocalVaultFromKey(keyA)
	if err != nil {
		t.Fatalf("vault A: %v", err)
	}
	b, err := NewLocalVaultFromKey(keyB)
	if err != nil {
		t.Fatalf("vault B: %v", err)
	}
	aad := []byte("u|svc")
	enc, iv, tag, err := a.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := b.Decrypt(enc, iv, tag, aad); err == nil {
		t.Fatalf("expected decrypt to fail under a different key")
	}
}

func TestNewLocalVaultFromKey_RejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33, 64} {
		key := make([]byte, n)
		if _, err := NewLocalVaultFromKey(key); err == nil {
			t.Errorf("expected error for key length %d", n)
		}
	}
}

func TestResolveKey_PrioritisesEnvOverFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "vault.key")
	fileKey := bytes.Repeat([]byte{0xAA}, 32)
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(fileKey)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	envKey := bytes.Repeat([]byte{0xBB}, 32)
	got, err := ResolveKey(base64.StdEncoding.EncodeToString(envKey), keyFile)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if !bytes.Equal(got, envKey) {
		t.Fatalf("expected env key to win over file key")
	}
}

func TestResolveKey_FailsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.key")
	if _, err := ResolveKey("", missing); err == nil {
		t.Fatalf("expected error for missing key file with no env override")
	}
}

func TestLoadOrCreateKey_RejectsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "vault.key")
	key := bytes.Repeat([]byte{0xCC}, 32)
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(key)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := loadOrCreateKey(keyFile)
	if err == nil || !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("expected insecure-permissions error, got %v", err)
	}
}

func TestLoadOrCreateKey_GeneratesNewKeyWith0600(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "vault.key")
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		t.Fatalf("loadOrCreateKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key))
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected key file permissions 0600, got %04o", perm)
	}
}

// TestLocalVault_DBBacked_RoundTrip exercises Set/Get/Delete/List against a
// minimal in-memory SQLite schema that mirrors the production vault_entries
// table.
func TestLocalVault_DBBacked_RoundTrip(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE vault_entries (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			service_id TEXT NOT NULL,
			encrypted  TEXT NOT NULL,
			iv         TEXT NOT NULL,
			auth_tag   TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, service_id)
		);
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	v, err := NewLocalVaultFromKeyWithDB(newKey(t), db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	plaintext := []byte(`{"access_token":"secret-value"}`)
	if err := v.Set(ctx, "u1", "gmail", plaintext); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := v.Get(ctx, "u1", "gmail")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Get mismatch: want %q got %q", plaintext, got)
	}

	// Overwrite path exercises ON CONFLICT DO UPDATE.
	updated := []byte(`{"access_token":"rotated-value"}`)
	if err := v.Set(ctx, "u1", "gmail", updated); err != nil {
		t.Fatalf("Set (update): %v", err)
	}
	got, err = v.Get(ctx, "u1", "gmail")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if !bytes.Equal(got, updated) {
		t.Fatalf("update not visible: want %q got %q", updated, got)
	}

	// Cross-user isolation.
	if _, err := v.Get(ctx, "u2", "gmail"); err == nil {
		t.Fatalf("expected ErrNotFound for other user, got nil")
	} else if err != pkgvault.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := v.Set(ctx, "u1", "calendar", []byte("c")); err != nil {
		t.Fatalf("Set calendar: %v", err)
	}
	services, err := v.List(ctx, "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(services) != 2 || services[0] != "calendar" || services[1] != "gmail" {
		t.Fatalf("List returned %v, want [calendar gmail]", services)
	}

	if err := v.Delete(ctx, "u1", "calendar"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := v.Get(ctx, "u1", "calendar"); err != pkgvault.ErrNotFound {
		t.Fatalf("after delete expected ErrNotFound, got %v", err)
	}
}

// TestLocalVault_CrossRowSwapFails proves that a DB-write attacker who copies
// (encrypted, iv, auth_tag) from user A's row into user B's row cannot trick
// Get into returning A's plaintext. The AAD binding makes the swap detectable.
func TestLocalVault_CrossRowSwapFails(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE vault_entries (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			service_id TEXT NOT NULL,
			encrypted  TEXT NOT NULL,
			iv         TEXT NOT NULL,
			auth_tag   TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, service_id)
		);
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	v, err := NewLocalVaultFromKeyWithDB(newKey(t), db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	if err := v.Set(ctx, "alice", "gmail", []byte("alice-token")); err != nil {
		t.Fatalf("Set alice: %v", err)
	}
	if err := v.Set(ctx, "bob", "gmail", []byte("bob-token")); err != nil {
		t.Fatalf("Set bob: %v", err)
	}

	// Simulate a privileged-DB attacker copying alice's ciphertext into bob's row.
	if _, err := db.ExecContext(ctx, `
		UPDATE vault_entries
		   SET encrypted = (SELECT encrypted FROM vault_entries WHERE user_id = 'alice' AND service_id = 'gmail'),
		       iv        = (SELECT iv        FROM vault_entries WHERE user_id = 'alice' AND service_id = 'gmail'),
		       auth_tag  = (SELECT auth_tag  FROM vault_entries WHERE user_id = 'alice' AND service_id = 'gmail')
		 WHERE user_id = 'bob' AND service_id = 'gmail'
	`); err != nil {
		t.Fatalf("swap: %v", err)
	}

	// Bob's Get must now fail rather than return alice's plaintext.
	got, err := v.Get(ctx, "bob", "gmail")
	if err == nil {
		t.Fatalf("expected swap to fail decryption; got plaintext %q", got)
	}
	if bytes.Equal(got, []byte("alice-token")) {
		t.Fatalf("AAD binding broken: bob's Get returned alice's plaintext")
	}
}

// TestLocalVault_LegacyRowMigratesLazily simulates a row written before the
// AAD binding shipped (sealed with empty AAD) and proves Get still resolves it
// so existing deployments don't lock users out.
func TestLocalVault_LegacyRowMigratesLazily(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE vault_entries (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			service_id TEXT NOT NULL,
			encrypted  TEXT NOT NULL,
			iv         TEXT NOT NULL,
			auth_tag   TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, service_id)
		);
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	v, err := NewLocalVaultFromKeyWithDB(newKey(t), db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	// Encrypt with empty AAD to mimic a pre-fix row.
	enc, iv, tag, err := v.Encrypt([]byte("legacy-secret"), nil)
	if err != nil {
		t.Fatalf("Encrypt legacy: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO vault_entries (id, user_id, service_id, encrypted, iv, auth_tag) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy-id", "alice", "gmail", enc, iv, tag,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	got, err := v.Get(ctx, "alice", "gmail")
	if err != nil {
		t.Fatalf("Get legacy: %v", err)
	}
	if !bytes.Equal(got, []byte("legacy-secret")) {
		t.Fatalf("legacy plaintext mismatch: %q", got)
	}
}
