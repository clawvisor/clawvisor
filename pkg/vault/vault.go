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

// ConditionalUpdater atomically replaces an existing credential only while its
// decrypted contents still equal expected. False means changed or deleted;
// implementations must never recreate a missing credential.
type ConditionalUpdater interface {
	CompareAndSwap(ctx context.Context, userID, serviceID string, expected, replacement []byte) (bool, error)
}

// ConditionalCredential identifies the real owner of a resolved credential.
// Updater is nil for read-only references or backends without atomic updates.
type ConditionalCredential struct {
	Value     []byte
	UserID    string
	ServiceID string
	Updater   ConditionalUpdater
}

// ConditionalReader lets routing wrappers preserve ownership and write policy
// when loading credentials for a conditional update. Unknown wrappers are never
// unwrapped implicitly.
type ConditionalReader interface {
	GetForUpdate(context.Context, string, string) (*ConditionalCredential, error)
}

func ReadForConditionalUpdate(ctx context.Context, v Vault, userID, serviceID string) (*ConditionalCredential, error) {
	if reader, ok := v.(ConditionalReader); ok {
		return reader.GetForUpdate(ctx, userID, serviceID)
	}
	raw, err := v.Get(ctx, userID, serviceID)
	if err != nil {
		return nil, err
	}
	updater, _ := v.(ConditionalUpdater)
	return &ConditionalCredential{Value: raw, UserID: userID, ServiceID: serviceID, Updater: updater}, nil
}

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
