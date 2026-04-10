package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"

	pkgvault "github.com/clawvisor/clawvisor/pkg/vault"
)

// LocalVault encrypts credentials with AES-256-GCM and stores them in the
// application database (Postgres or SQLite). The 32-byte master key is read
// from a file on disk and never stored in the database or config.
type LocalVault struct {
	key    []byte
	db     *sql.DB
	driver string // "postgres" or "sqlite"
}

// NewLocalVault initialises a LocalVault from a key file.
// If the key file does not exist it is created with a freshly generated key.
// driver must be "postgres" or "sqlite".
func NewLocalVault(keyFile string, db *sql.DB, driver string) (*LocalVault, error) {
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return nil, err
	}
	return &LocalVault{key: key, db: db, driver: driver}, nil
}

// NewLocalVaultFromKey builds a LocalVault from a raw 32-byte key.
// Useful for the GCP vault which manages its own key storage.
func NewLocalVaultFromKey(key []byte) (*LocalVault, error) {
	if len(key) != 32 {
		return nil, errors.New("vault key must be exactly 32 bytes")
	}
	return &LocalVault{key: key}, nil
}

// ph returns the correct positional placeholder for the driver.
// Postgres uses $N, SQLite uses ?.
func (v *LocalVault) ph(n int) string {
	if v.driver == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func (v *LocalVault) Set(ctx context.Context, userID, serviceID string, credential []byte) error {
	encrypted, iv, authTag, err := v.encrypt(credential)
	if err != nil {
		return fmt.Errorf("vault encrypt: %w", err)
	}
	id := uuid.New().String()

	var query string
	if v.driver == "postgres" {
		query = `
			INSERT INTO vault_entries (id, user_id, service_id, encrypted, iv, auth_tag)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id, service_id) DO UPDATE SET
				encrypted  = EXCLUDED.encrypted,
				iv         = EXCLUDED.iv,
				auth_tag   = EXCLUDED.auth_tag,
				updated_at = NOW()`
	} else {
		query = `
			INSERT INTO vault_entries (id, user_id, service_id, encrypted, iv, auth_tag)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, service_id) DO UPDATE SET
				encrypted  = excluded.encrypted,
				iv         = excluded.iv,
				auth_tag   = excluded.auth_tag,
				updated_at = CURRENT_TIMESTAMP`
	}
	_, err = v.db.ExecContext(ctx, query, id, userID, serviceID, encrypted, iv, authTag)
	return err
}

func (v *LocalVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	q := fmt.Sprintf(
		`SELECT encrypted, iv, auth_tag FROM vault_entries WHERE user_id = %s AND service_id = %s`,
		v.ph(1), v.ph(2),
	)
	var encrypted, iv, authTag string
	err := v.db.QueryRowContext(ctx, q, userID, serviceID).Scan(&encrypted, &iv, &authTag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, pkgvault.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("vault get: %w", err)
	}
	return v.decrypt(encrypted, iv, authTag)
}

func (v *LocalVault) Delete(ctx context.Context, userID, serviceID string) error {
	q := fmt.Sprintf(
		`DELETE FROM vault_entries WHERE user_id = %s AND service_id = %s`,
		v.ph(1), v.ph(2),
	)
	_, err := v.db.ExecContext(ctx, q, userID, serviceID)
	return err
}

func (v *LocalVault) List(ctx context.Context, userID string) ([]string, error) {
	q := fmt.Sprintf(
		`SELECT service_id FROM vault_entries WHERE user_id = %s ORDER BY service_id`,
		v.ph(1),
	)
	rows, err := v.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("vault list: %w", err)
	}
	defer rows.Close()

	var services []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		services = append(services, s)
	}
	return services, rows.Err()
}

// Encrypt and Decrypt are exported so GCPVault can reuse the AES logic.

func (v *LocalVault) Encrypt(plaintext []byte) (encrypted, iv, authTag string, err error) {
	return v.encrypt(plaintext)
}

func (v *LocalVault) Decrypt(encrypted, iv, authTag string) ([]byte, error) {
	return v.decrypt(encrypted, iv, authTag)
}

func (v *LocalVault) encrypt(plaintext []byte) (encrypted, iv, authTag string, err error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return "", "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", "", err
	}
	// Seal appends auth tag after ciphertext
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	tagSize := gcm.Overhead()
	cipherBytes := sealed[:len(sealed)-tagSize]
	tagBytes := sealed[len(sealed)-tagSize:]

	return base64.StdEncoding.EncodeToString(cipherBytes),
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(tagBytes),
		nil
}

func (v *LocalVault) decrypt(encrypted, iv, authTag string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(iv)
	if err != nil {
		return nil, fmt.Errorf("decode iv: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(authTag)
	if err != nil {
		return nil, fmt.Errorf("decode auth tag: %w", err)
	}
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := append(ciphertext, tag...) //nolint:gocritic
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("vault decrypt (tampered data?): %w", err)
	}
	return plaintext, nil
}

// NewLocalVaultFromKeyWithDB builds a LocalVault from a raw 32-byte key with
// a database connection. Use this when the key is resolved externally (e.g.
// from an environment variable) but the vault still needs DB-backed storage.
func NewLocalVaultFromKeyWithDB(key []byte, db *sql.DB, driver string) (*LocalVault, error) {
	if len(key) != 32 {
		return nil, errors.New("vault key must be exactly 32 bytes")
	}
	return &LocalVault{key: key, db: db, driver: driver}, nil
}

// ResolveKey resolves the 32-byte vault master key with clear priority:
//   - If masterKeyB64 is non-empty, base64-decode it and validate 32 bytes
//   - Otherwise, read from keyFile (must already exist)
//
// Unlike loadOrCreateKey, this function never auto-generates a key.
func ResolveKey(masterKeyB64, keyFile string) ([]byte, error) {
	if masterKeyB64 != "" {
		key, err := base64.StdEncoding.DecodeString(masterKeyB64)
		if err != nil {
			return nil, fmt.Errorf("decoding VAULT_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("VAULT_KEY must decode to exactly 32 bytes, got %d", len(key))
		}
		return key, nil
	}
	data, err := os.ReadFile(keyFile)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("vault key not configured: set VAULT_KEY env var or create key file %s", keyFile)
	}
	if err != nil {
		return nil, fmt.Errorf("reading vault key file: %w", err)
	}
	key, decErr := base64.StdEncoding.DecodeString(string(data))
	if decErr != nil {
		return nil, fmt.Errorf("vault key file %s: invalid base64: %w", keyFile, decErr)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("vault key file %s: expected 32 bytes, got %d", keyFile, len(key))
	}
	return key, nil
}

// LoadKey is the exported version of loadOrCreateKey, used by main.go
// when the GCP vault backend needs the master key.
func LoadKey(path string) ([]byte, error) {
	return loadOrCreateKey(path)
}

func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		// Validate file permissions — the key file should not be readable by
		// group or others. Skip the check on Windows where Unix permissions
		// don't apply (FileMode always reports 0666).
		if info, statErr := os.Stat(path); statErr == nil {
			if perm := info.Mode().Perm(); perm&0077 != 0 {
				return nil, fmt.Errorf("vault key file %s has insecure permissions %04o (expected 0600)", path, perm)
			}
		}
		key, decErr := base64.StdEncoding.DecodeString(string(data))
		if decErr != nil {
			return nil, fmt.Errorf("vault key file %s: invalid base64: %w", path, decErr)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("vault key file %s: expected 32 bytes, got %d", path, len(key))
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading vault key file: %w", err)
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generating vault key: %w", err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		return nil, fmt.Errorf("writing vault key file: %w", err)
	}
	return key, nil
}
