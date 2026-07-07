package vault

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

// wrappedNotFoundVault fronts a real backend but wraps ErrNotFound from Get
// (as a spec-10 reference backend legitimately might). It proves the shared
// fallback uses errors.Is, not `==`.
type wrappedNotFoundVault struct{ Vault }

func (w wrappedNotFoundVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	b, err := w.Vault.Get(ctx, userID, serviceID)
	if err == ErrNotFound {
		return nil, fmt.Errorf("backend miss for %s/%s: %w", userID, serviceID, err)
	}
	return b, err
}

// TestInstanceVault_WrappedNotFoundStillFallsBack: a backend that wraps
// ErrNotFound must not defeat the specific→shared fallback. Under the old
// `err != ErrNotFound` check the wrapped error would short-circuit and
// alice would never see the shared key.
func TestInstanceVault_WrappedNotFoundStillFallsBack(t *testing.T) {
	inner, ctx := newDBVault(t)
	iv := NewInstanceAware(wrappedNotFoundVault{inner})

	shared := []byte("shared-anthropic-key")
	if err := inner.Set(ctx, InstanceUserID, "anthropic", shared); err != nil {
		t.Fatalf("seed shared: %v", err)
	}

	got, err := iv.Get(ctx, "alice", "anthropic")
	if err != nil {
		t.Fatalf("alice Get (wrapped-notfound backend): %v", err)
	}
	if !bytes.Equal(got, shared) {
		t.Fatalf("alice got %q, want shared fallback", got)
	}
}

func newDBVault(t *testing.T) (*LocalVault, context.Context) {
	t.Helper()
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
		);`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	v, err := NewLocalVaultFromKeyWithDB(newKey(t), db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return v, ctx
}

// TestInstanceVault_ResolutionOrder: a shared entry set under `_instance`
// is returned to a member with no personal entry; a member with a personal
// entry keeps their own (user-specific wins).
func TestInstanceVault_ResolutionOrder(t *testing.T) {
	inner, ctx := newDBVault(t)
	iv := NewInstanceAware(inner)

	shared := []byte("shared-anthropic-key")
	if err := inner.Set(ctx, InstanceUserID, "anthropic", shared); err != nil {
		t.Fatalf("seed shared: %v", err)
	}

	// alice has no personal entry → gets the shared one.
	got, err := iv.Get(ctx, "alice", "anthropic")
	if err != nil {
		t.Fatalf("alice Get: %v", err)
	}
	if !bytes.Equal(got, shared) {
		t.Fatalf("alice got %q, want shared", got)
	}

	// bob has a personal entry → user-specific wins over shared.
	personal := []byte("bobs-own-key")
	if err := inner.Set(ctx, "bob", "anthropic", personal); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	got, err = iv.Get(ctx, "bob", "anthropic")
	if err != nil {
		t.Fatalf("bob Get: %v", err)
	}
	if !bytes.Equal(got, personal) {
		t.Fatalf("bob got %q, want personal", got)
	}

	// No shared entry for a different service → ErrNotFound bubbles up.
	if _, err := iv.Get(ctx, "alice", "openai"); err != ErrNotFound {
		t.Fatalf("missing service: err=%v want ErrNotFound", err)
	}

	// alice's listing surfaces the shared id even without a personal entry.
	ids, err := iv.List(ctx, "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 1 || ids[0] != "anthropic" {
		t.Fatalf("alice list = %v, want [anthropic]", ids)
	}
}

// TestInstanceVault_AADPromotion: promoting a personal credential to shared
// via decrypt-then-reencrypt under `_instance` decrypts cleanly, while a raw
// row-copy of the personal ciphertext into the `_instance` row fails
// decryption (the AAD-binding regression guard — gotcha #5).
func TestInstanceVault_AADPromotion(t *testing.T) {
	inner, ctx := newDBVault(t)

	secret := []byte("promote-me")
	if err := inner.Set(ctx, "alice", "anthropic", secret); err != nil {
		t.Fatalf("seed personal: %v", err)
	}

	// Correct promotion: Get (decrypt under alice AAD) then Set under
	// _instance (re-encrypt under _instance AAD).
	plain, err := inner.Get(ctx, "alice", "anthropic")
	if err != nil {
		t.Fatalf("get personal: %v", err)
	}
	if err := inner.Set(ctx, InstanceUserID, "anthropic", plain); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	got, err := inner.Get(ctx, InstanceUserID, "anthropic")
	if err != nil {
		t.Fatalf("get shared after promotion: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("promoted shared got %q, want %q", got, secret)
	}

	// Regression guard: a raw DB row-copy of alice's ciphertext into an
	// `_instance` row must NOT decrypt (AES-GCM AAD is bound to
	// user_id|service_id — you cannot re-attribute by copying the row).
	db := inner.db
	if _, err := db.ExecContext(ctx, `
		INSERT INTO vault_entries (id, user_id, service_id, encrypted, iv, auth_tag)
		SELECT 'copied', '_instance', 'rowcopy', encrypted, iv, auth_tag
		FROM vault_entries WHERE user_id='alice' AND service_id='anthropic'`); err != nil {
		t.Fatalf("row copy: %v", err)
	}
	if _, err := inner.Get(ctx, InstanceUserID, "rowcopy"); err == nil {
		t.Fatal("expected raw row-copy to fail decryption, but Get succeeded")
	}
}
