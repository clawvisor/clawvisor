package vault

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a vault entry does not exist.
var ErrNotFound = errors.New("vault: credential not found")

// ErrAlreadyExists is returned by SetIfAbsent when a credential is
// already stored for the (userID, serviceID) pair.
var ErrAlreadyExists = errors.New("vault: credential already exists")

// Vault stores and retrieves encrypted service credentials.
// Credentials are scoped per-user per-service. The agent never interacts
// with the vault directly and never receives credential values in responses.
type Vault interface {
	// Set encrypts and stores a credential for (userID, serviceID).
	// Overwrites any existing credential for that pair.
	Set(ctx context.Context, userID, serviceID string, credential []byte) error

	// SetIfAbsent atomically stores a credential only if no credential
	// exists for (userID, serviceID). Returns ErrAlreadyExists when the
	// pair is already populated. Use this for create-only handler paths
	// where a check-then-Set would race against concurrent creators.
	SetIfAbsent(ctx context.Context, userID, serviceID string, credential []byte) error

	// Get retrieves and decrypts the credential for (userID, serviceID).
	// Returns ErrNotFound if no credential exists.
	Get(ctx context.Context, userID, serviceID string) ([]byte, error)

	// Delete removes the credential for (userID, serviceID).
	// No-op if it doesn't exist.
	Delete(ctx context.Context, userID, serviceID string) error

	// List returns the serviceIDs that have stored credentials for userID.
	List(ctx context.Context, userID string) ([]string, error)
}
