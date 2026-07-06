package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func newAPITokenStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db), ctx
}

// TestAPITokenStore_CRUD exercises create → get-by-hash → list → revoke,
// including the nullable-timestamp scans and the is_bootstrap flag.
func TestAPITokenStore_CRUD(t *testing.T) {
	st, ctx := newAPITokenStore(t)

	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	tok := &store.APIToken{
		Name:        "terraform",
		TokenHash:   "hash-abc",
		TokenPrefix: "cvat_AbC1dEf2gH",
		Scope:       "instance-admin",
		ExpiresAt:   &exp,
		IsBootstrap: true,
	}
	if err := st.CreateAPIToken(ctx, tok); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if tok.ID == "" {
		t.Fatal("CreateAPIToken did not assign an ID")
	}

	got, err := st.GetAPITokenByHash(ctx, "hash-abc")
	if err != nil {
		t.Fatalf("GetAPITokenByHash: %v", err)
	}
	if got.Name != "terraform" || got.Scope != "instance-admin" || got.TokenPrefix != "cvat_AbC1dEf2gH" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.IsBootstrap {
		t.Fatal("is_bootstrap did not round-trip")
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at mismatch: got %v want %v", got.ExpiresAt, exp)
	}
	if got.RevokedAt != nil || got.LastUsedAt != nil {
		t.Fatalf("expected nil revoked_at/last_used_at, got %+v", got)
	}

	// Touch last_used_at.
	if err := st.TouchAPITokenLastUsed(ctx, tok.ID); err != nil {
		t.Fatalf("TouchAPITokenLastUsed: %v", err)
	}
	got, _ = st.GetAPITokenByHash(ctx, "hash-abc")
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at not set after touch")
	}

	// List.
	list, err := st.ListAPITokens(ctx)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListAPITokens len = %d, want 1", len(list))
	}

	// Revoke → GetAPITokenByHash still returns the row (soft delete) with
	// revoked_at set.
	if err := st.RevokeAPIToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	got, _ = st.GetAPITokenByHash(ctx, "hash-abc")
	if got.RevokedAt == nil {
		t.Fatal("revoked_at not set after revoke")
	}

	// Revoke unknown → ErrNotFound.
	if err := st.RevokeAPIToken(ctx, "does-not-exist"); err != store.ErrNotFound {
		t.Fatalf("RevokeAPIToken(unknown) = %v, want ErrNotFound", err)
	}

	// Missing hash → ErrNotFound.
	if _, err := st.GetAPITokenByHash(ctx, "nope"); err != store.ErrNotFound {
		t.Fatalf("GetAPITokenByHash(missing) = %v, want ErrNotFound", err)
	}
}

// TestAPITokenStore_InstanceSeededAndExcludedFromCount verifies the
// migration seeds the `_instance` user and that CountUsers excludes it
// (so it never trips first-user onboarding detection).
func TestAPITokenStore_InstanceSeededAndExcludedFromCount(t *testing.T) {
	st, ctx := newAPITokenStore(t)

	u, err := st.GetUserByID(ctx, store.InstanceUserID)
	if err != nil {
		t.Fatalf("GetUserByID(_instance): %v", err)
	}
	if u.Email != "instance@system.clawvisor.invalid" {
		t.Fatalf("_instance email = %q", u.Email)
	}

	n, err := st.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountUsers = %d, want 0 (only _instance/__system__ seeded)", n)
	}

	if _, err := st.CreateUser(ctx, "real@example.com", "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	n, _ = st.CountUsers(ctx)
	if n != 1 {
		t.Fatalf("CountUsers after real user = %d, want 1", n)
	}
}
