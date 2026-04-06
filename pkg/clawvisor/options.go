// Package clawvisor provides the public API for embedding and extending
// the Clawvisor server. Cloud and enterprise builds import this package
// to customize behavior while reusing the open-source core.
package clawvisor

import (
	"context"
	"crypto/ecdh"
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/internal/notify/push"
	"github.com/clawvisor/clawvisor/internal/relay"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// GatewayHooks allows cloud/enterprise layers to inject additional
// authorization logic into the gateway request flow.
type GatewayHooks struct {
	// BeforeAuthorize is called after request parsing, before restriction checks.
	// The agent (including OrgID) is available via middleware.AgentFromContext(ctx).
	// Return a non-nil error to block the request (treated as an org policy block).
	BeforeAuthorize func(ctx context.Context, agentID, userID, service, action string) error
}

// ServerOptions holds everything needed to start a Clawvisor server.
// Use DefaultOptions to get the standard open-source defaults, then
// selectively override fields before passing to Run.
type ServerOptions struct {
	Logger *slog.Logger
	Config *config.Config

	// Core dependencies.
	Store      store.Store
	Vault      vault.Vault
	JWTService auth.TokenService
	AdapterReg *adapters.Registry
	Notifier   notify.Notifier

	// RelayClient connects to the cloud relay for public internet access.
	// Leave nil to disable relay (localhost-only operation).
	RelayClient *relay.Client

	// X25519Key is the daemon's X25519 private key for E2E encryption.
	// Required when relay is enabled. Used by E2E middleware on gateway routes.
	X25519Key *ecdh.PrivateKey

	// PushNotifier is the concrete push notifier for device registration and
	// action handling. Set when push notifications are enabled.
	// The Notifier field should be a MultiNotifier wrapping both Telegram and Push.
	PushNotifier *push.Notifier

	// MessageBuffer stores recent group chat messages for on-demand LLM
	// approval checking. Set when group observation is enabled.
	MessageBuffer *groupchat.MessageBuffer

	// MagicStore enables magic-link auth (local mode).
	// Leave nil to disable.
	MagicStore auth.MagicTokenStore

	// Features declares which capabilities the frontend exposes.
	Features FeatureSet

	// ExtraRoutes registers additional HTTP routes (e.g. cloud-only endpoints).
	ExtraRoutes func(mux *http.ServeMux, deps Dependencies)

	// WrapRoutes wraps the entire HTTP handler (e.g. tenant-scoping middleware).
	WrapRoutes func(handler http.Handler) http.Handler

	// SkipBuiltinAuth prevents the core server from registering its built-in
	// login/register/password routes, allowing ExtraRoutes to provide custom auth.
	SkipBuiltinAuth bool

	// GatewayHooks injects additional authorization logic into the gateway flow.
	// Used by cloud for org-level restrictions, policies, etc.
	GatewayHooks *GatewayHooks

	// Quiet suppresses user-facing messages and sets server log level to WARN.
	// Used during daemon setup when a temporary server runs in the background.
	Quiet bool
}

// Dependencies is passed to ExtraRoutes so extension handlers can access shared services.
type Dependencies struct {
	Store      store.Store
	Vault      vault.Vault
	JWTService auth.TokenService
	AdapterReg *adapters.Registry
	Notifier   notify.Notifier
	Logger     *slog.Logger
	BaseURL    string
}

// FeatureSet tells the frontend (and API consumers) which capabilities are available.
// The open-source build returns all false; the cloud build sets the relevant fields.
type FeatureSet struct {
	MultiTenant       bool `json:"multi_tenant"`
	EmailVerification bool `json:"email_verification"`
	Passkeys          bool `json:"passkeys"`
	SSO               bool `json:"sso"`
	Teams             bool `json:"teams"`
	UsageMetering     bool `json:"usage_metering"`
	PasswordAuth      bool `json:"password_auth"`
}
