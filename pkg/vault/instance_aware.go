package vault

import (
	"context"
	"errors"
)

// InstanceUserID is the owner of instance-shared vault entries. It mirrors
// store.InstanceUserID (the `_instance` system user seeded by 05-lite); it
// is redeclared here rather than imported so pkg/vault stays free of a
// dependency on pkg/store. The two MUST stay in sync.
const InstanceUserID = "_instance"

// InstanceAwareVault wraps a backing Vault and resolves reads
// specific-first-then-shared, mirroring cloud's OrgAwareVault. A team can
// share one Anthropic key by storing it under `_instance`; a member with no
// personal entry transparently gets the shared one, while a member with a
// personal entry keeps using their own.
//
// Writes are delegated unchanged (a caller's userID is preserved) — shared
// entries are only ever written by the dedicated /api/vault/shared endpoints
// passing InstanceUserID explicitly, so LocalVault seals them under the
// `_instance|<serviceID>` AAD automatically.
//
// It wraps unconditionally: with no shared entries it is a transparent
// passthrough. The cloud repo wraps this again with OrgAwareVault, giving a
// correct org → user → instance resolution order.
type InstanceAwareVault struct {
	inner Vault
}

// NewInstanceAware wraps inner so instance-shared entries resolve as a
// fallback behind user-specific ones.
func NewInstanceAware(inner Vault) *InstanceAwareVault {
	return &InstanceAwareVault{inner: inner}
}

// Get returns the user-specific credential when present, otherwise falls
// back to the instance-shared one. User-specific always wins.
func (v *InstanceAwareVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	cred, err := v.inner.Get(ctx, userID, serviceID)
	if err == nil {
		return cred, nil
	}
	if !errors.Is(err, ErrNotFound) || userID == InstanceUserID {
		return nil, err
	}
	return v.inner.Get(ctx, InstanceUserID, serviceID)
}

// Set delegates unchanged (shared writes go through the dedicated endpoints
// with InstanceUserID as the explicit owner).
func (v *InstanceAwareVault) Set(ctx context.Context, userID, serviceID string, credential []byte) error {
	return v.inner.Set(ctx, userID, serviceID, credential)
}

// SetIfAbsent delegates unchanged.
func (v *InstanceAwareVault) SetIfAbsent(ctx context.Context, userID, serviceID string, credential []byte) error {
	return v.inner.SetIfAbsent(ctx, userID, serviceID, credential)
}

// Delete delegates unchanged.
func (v *InstanceAwareVault) Delete(ctx context.Context, userID, serviceID string) error {
	return v.inner.Delete(ctx, userID, serviceID)
}

// List returns the union of the user's own service IDs and the shared ones,
// so shared entries surface in a member's listing. The user's own IDs take
// precedence (a shared ID already present personally is not duplicated).
func (v *InstanceAwareVault) List(ctx context.Context, userID string) ([]string, error) {
	own, err := v.inner.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	if userID == InstanceUserID {
		return own, nil
	}
	shared, err := v.inner.List(ctx, InstanceUserID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(own))
	out := make([]string, 0, len(own)+len(shared))
	for _, id := range own {
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range shared {
		if _, ok := seen[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// SharedList returns the service IDs of instance-shared entries. It is a
// convenience for the /api/vault/shared surface; equivalent to
// inner.List(InstanceUserID).
func (v *InstanceAwareVault) SharedList(ctx context.Context) ([]string, error) {
	return v.inner.List(ctx, InstanceUserID)
}

// compile-time assertion.
var _ Vault = (*InstanceAwareVault)(nil)
