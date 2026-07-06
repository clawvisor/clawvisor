package clawvisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// bootstrapTokenEnv is the env var the deploy module (spec 03) delivers the
// first-boot bootstrap token through. The module GENERATES the value
// (random_password → cvat_ + 43 base64url chars); the server never
// generates it and never prints it.
const bootstrapTokenEnv = "CLAWVISOR_BOOTSTRAP_TOKEN"

// bootstrapTokenTTL is the mandatory hard expiry of the bootstrap token.
// The token exists in plaintext in the secret manager, the compose env on
// disk, and Terraform state, so it must be short-lived by construction.
const bootstrapTokenTTL = 24 * time.Hour

// bootstrapAPIToken seeds the first-boot bootstrap API token from
// CLAWVISOR_BOOTSTRAP_TOKEN. It is idempotent across restarts and never
// overrides a live install. Called from the startup path after store init.
//
// Contract (coordinates with spec 03's Terraform module):
//  1. unset            → no-op.
//  2. malformed value  → refuse to start (misconfiguration, not a warning).
//  3. hash already present → no-op (idempotent; may already be revoked).
//  4. a non-revoked instance-admin token already exists → warn and skip.
//  5. else insert: name="bootstrap", scope=instance-admin, created_by=NULL,
//     expires_at = now + 24h, is_bootstrap=true (burn-on-use target).
func bootstrapAPIToken(ctx context.Context, st store.Store, logger *slog.Logger) error {
	val := os.Getenv(bootstrapTokenEnv)
	if val == "" {
		return nil
	}
	if !auth.ValidAPITokenFormat(val) {
		return fmt.Errorf("%s is malformed: must match cvat_ + 43 base64url chars", bootstrapTokenEnv)
	}

	hash := auth.HashToken(val)
	if _, err := st.GetAPITokenByHash(ctx, hash); err == nil {
		// Idempotent across restarts. The row may already be revoked
		// (burn-on-use) — that is fine, we still do not re-seed it.
		logger.Info("bootstrap API token already present; skipping seed")
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("checking for existing bootstrap token: %w", err)
	}

	tokens, err := st.ListAPITokens(ctx)
	if err != nil {
		return fmt.Errorf("listing api tokens: %w", err)
	}
	for _, t := range tokens {
		if t.Scope == middleware.ScopeInstanceAdmin && t.RevokedAt == nil {
			logger.Warn("a non-revoked instance-admin API token already exists; refusing to seed bootstrap token over a live install")
			return nil
		}
	}

	expiresAt := time.Now().Add(bootstrapTokenTTL).UTC()
	tok := &store.APIToken{
		Name:        "bootstrap",
		TokenHash:   hash,
		TokenPrefix: val[:auth.APITokenPrefixLen],
		Scope:       middleware.ScopeInstanceAdmin,
		CreatedBy:   nil,
		ExpiresAt:   &expiresAt,
		IsBootstrap: true,
	}
	if err := st.CreateAPIToken(ctx, tok); err != nil {
		return fmt.Errorf("seeding bootstrap token: %w", err)
	}
	logger.Info("seeded bootstrap API token (single-use, first mint burns it)", "expires_at", expiresAt.Format(time.RFC3339))
	return nil
}
