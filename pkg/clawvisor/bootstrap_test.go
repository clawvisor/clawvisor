package clawvisor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newBootstrapStore(t *testing.T) (store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "boot.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db), ctx
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// genBootstrapToken returns a valid cvat_ value for use as a bootstrap
// token in tests.
func genBootstrapToken(t *testing.T) string {
	t.Helper()
	raw, _, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	return raw
}

// TestBootstrap_MintOnce: a valid bootstrap token seeds exactly one
// instance-admin, is_bootstrap row with a +24h expiry.
func TestBootstrap_MintOnce(t *testing.T) {
	st, ctx := newBootstrapStore(t)
	raw := genBootstrapToken(t)
	t.Setenv(bootstrapTokenEnv, raw)

	if err := bootstrapAPIToken(ctx, st, quietLogger()); err != nil {
		t.Fatalf("bootstrapAPIToken: %v", err)
	}
	got, err := st.GetAPITokenByHash(ctx, auth.HashToken(raw))
	if err != nil {
		t.Fatalf("GetAPITokenByHash: %v", err)
	}
	if got.Name != "bootstrap" || got.Scope != middleware.ScopeInstanceAdmin || !got.IsBootstrap {
		t.Fatalf("unexpected bootstrap row: %+v", got)
	}
	if got.CreatedBy != nil {
		t.Fatalf("bootstrap created_by should be NULL, got %v", *got.CreatedBy)
	}
	if got.ExpiresAt == nil {
		t.Fatal("bootstrap token must have a mandatory +24h expiry")
	}
}

// TestBootstrap_Idempotent: re-running with the same token is a no-op and
// does not create a duplicate row (idempotent across restarts). It stays
// a no-op even when the existing row is already revoked (burned).
func TestBootstrap_Idempotent(t *testing.T) {
	st, ctx := newBootstrapStore(t)
	raw := genBootstrapToken(t)
	t.Setenv(bootstrapTokenEnv, raw)

	if err := bootstrapAPIToken(ctx, st, quietLogger()); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	first, _ := st.GetAPITokenByHash(ctx, auth.HashToken(raw))

	// Simulate burn-on-use, then re-run: must remain a no-op (not re-seed).
	if err := st.RevokeAPIToken(ctx, first.ID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if err := bootstrapAPIToken(ctx, st, quietLogger()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	list, _ := st.ListAPITokens(ctx)
	if len(list) != 1 {
		t.Fatalf("expected 1 token after idempotent re-run, got %d", len(list))
	}
	again, _ := st.GetAPITokenByHash(ctx, auth.HashToken(raw))
	if again.RevokedAt == nil {
		t.Fatal("re-run must not resurrect a burned bootstrap token")
	}
}

// TestBootstrap_RefusesMalformed: a malformed value returns an error
// (the caller refuses to start).
func TestBootstrap_RefusesMalformed(t *testing.T) {
	st, ctx := newBootstrapStore(t)
	t.Setenv(bootstrapTokenEnv, "cvat_not-valid")
	if err := bootstrapAPIToken(ctx, st, quietLogger()); err == nil {
		t.Fatal("expected error for malformed bootstrap token")
	}
	list, _ := st.ListAPITokens(ctx)
	if len(list) != 0 {
		t.Fatalf("malformed token must not seed a row, got %d", len(list))
	}
}

// TestBootstrap_Unset: no env var → clean no-op.
func TestBootstrap_Unset(t *testing.T) {
	st, ctx := newBootstrapStore(t)
	t.Setenv(bootstrapTokenEnv, "")
	if err := bootstrapAPIToken(ctx, st, quietLogger()); err != nil {
		t.Fatalf("unset bootstrap should be a no-op, got %v", err)
	}
	list, _ := st.ListAPITokens(ctx)
	if len(list) != 0 {
		t.Fatalf("unset must not seed a row, got %d", len(list))
	}
}

// TestBootstrap_SkipsWhenAdminTokenExists: bootstrap never overrides a
// live install — if a non-revoked instance-admin token already exists, the
// seed is skipped.
func TestBootstrap_SkipsWhenAdminTokenExists(t *testing.T) {
	st, ctx := newBootstrapStore(t)

	// Pre-existing live instance-admin token (e.g. minted earlier).
	existing := &store.APIToken{
		Name:        "live-admin",
		TokenHash:   auth.HashToken("some-other-value"),
		TokenPrefix: "cvat_live0000000",
		Scope:       middleware.ScopeInstanceAdmin,
	}
	if err := st.CreateAPIToken(ctx, existing); err != nil {
		t.Fatalf("seed existing: %v", err)
	}

	raw := genBootstrapToken(t)
	t.Setenv(bootstrapTokenEnv, raw)
	if err := bootstrapAPIToken(ctx, st, quietLogger()); err != nil {
		t.Fatalf("bootstrapAPIToken: %v", err)
	}
	// The bootstrap value must NOT have been seeded.
	if _, err := st.GetAPITokenByHash(ctx, auth.HashToken(raw)); err != store.ErrNotFound {
		t.Fatalf("expected bootstrap skipped (ErrNotFound), got %v", err)
	}
}
