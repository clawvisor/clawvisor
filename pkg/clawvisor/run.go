package clawvisor

import (
	"context"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/clawvisor/clawvisor/internal/api"
)

// RunWithContext starts the Clawvisor server using the provided context for
// lifecycle management. The caller is responsible for cancellation and signal
// handling. Used by the daemon to control server lifetime during first-run
// service setup (where the server may need to be restarted).
func RunWithContext(ctx context.Context, opts *ServerOptions) error {
	var apiOpts []api.ServerOption

	apiOpts = append(apiOpts, api.WithFeatures(api.FeatureSet{
		MultiTenant:       opts.Features.MultiTenant,
		EmailVerification: opts.Features.EmailVerification,
		Passkeys:          opts.Features.Passkeys,
		SSO:               opts.Features.SSO,
		Teams:             opts.Features.Teams,
		UsageMetering:     opts.Features.UsageMetering,
		PasswordAuth:      opts.Features.PasswordAuth,
		Billing:           opts.Features.Billing,
	}))

	if opts.ExtraRoutes != nil {
		apiOpts = append(apiOpts, api.WithExtraRoutes(func(mux *http.ServeMux, deps api.Dependencies) {
			opts.ExtraRoutes(mux, Dependencies{
				Store:      deps.Store,
				Vault:      deps.Vault,
				JWTService: deps.JWTService,
				AdapterReg: deps.AdapterReg,
				Notifier:   deps.Notifier,
				Logger:     deps.Logger,
				BaseURL:    deps.BaseURL,
			})
		}))
	}

	if opts.WrapRoutes != nil {
		apiOpts = append(apiOpts, api.WithWrapRoutes(opts.WrapRoutes))
	}

	if opts.SkipBuiltinAuth {
		apiOpts = append(apiOpts, api.WithSkipBuiltinAuth())
	}

	if opts.Quiet {
		apiOpts = append(apiOpts, api.WithQuiet())
	}

	if opts.X25519Key != nil {
		apiOpts = append(apiOpts, api.WithE2EKey(opts.X25519Key))
	}

	if opts.Config.Relay.DaemonID != "" {
		apiOpts = append(apiOpts, api.WithDaemonKeys(
			opts.Config.Relay.DaemonID,
			opts.X25519Key,
		))
	}

	if opts.PushNotifier != nil {
		apiOpts = append(apiOpts, api.WithPushNotifier(opts.PushNotifier))
	}

	if opts.MessageBuffer != nil {
		apiOpts = append(apiOpts, api.WithGroupChatBuffer(opts.MessageBuffer))
	}

	if opts.EventHub != nil {
		apiOpts = append(apiOpts, api.WithEventHub(opts.EventHub))
	}

	if opts.DecisionBus != nil {
		apiOpts = append(apiOpts, api.WithDecisionBus(opts.DecisionBus))
	}

	if opts.AdapterGenFactory != nil {
		apiOpts = append(apiOpts, api.WithAdapterGenFactory(opts.AdapterGenFactory))
	}

	if opts.GatewayHooks != nil {
		apiOpts = append(apiOpts, api.WithGatewayHooks(&api.GatewayHooks{
			BeforeAuthorize: opts.GatewayHooks.BeforeAuthorize,
		}))
	}

	// Multi-instance Redis-backed stores.
	if opts.TicketStore != nil {
		apiOpts = append(apiOpts, api.WithTicketStore(opts.TicketStore))
	}
	if opts.ReplayCache != nil {
		apiOpts = append(apiOpts, api.WithReplayCache(opts.ReplayCache))
	}
	if opts.TokenCache != nil {
		apiOpts = append(apiOpts, api.WithTokenCache(opts.TokenCache))
	}
	if opts.DevicePairingStore != nil {
		apiOpts = append(apiOpts, api.WithDevicePairingStore(opts.DevicePairingStore))
	}
	if opts.OAuthStateStore != nil {
		apiOpts = append(apiOpts, api.WithOAuthStateStore(opts.OAuthStateStore))
	}
	if opts.PairingCodeStore != nil {
		apiOpts = append(apiOpts, api.WithPairingCodeStore(opts.PairingCodeStore))
	}
	if opts.DedupCache != nil {
		apiOpts = append(apiOpts, api.WithDedupCache(opts.DedupCache))
	}
	if opts.VerdictCache != nil {
		apiOpts = append(apiOpts, api.WithVerdictCache(opts.VerdictCache))
	}

	srv, err := api.New(
		opts.Config, opts.Store, opts.Vault, opts.JWTService,
		opts.AdapterReg, opts.Notifier, opts.Config.LLM, opts.MagicStore,
		apiOpts...,
	)
	if err != nil {
		return err
	}

	// Start relay client if configured. Give it the real server handler so
	// relay-proxied requests go through the full middleware stack.
	if opts.RelayClient != nil {
		opts.RelayClient.SetHandler(srv.Handler())
		go func() {
			if err := opts.RelayClient.Run(ctx); err != nil && ctx.Err() == nil {
				opts.Logger.Error("relay client stopped", "error", err)
			}
		}()
	}

	return srv.Run(ctx)
}

// Run starts the Clawvisor server with the given options and blocks until
// interrupted (SIGINT/SIGTERM).
func Run(opts *ServerOptions) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return RunWithContext(ctx, opts)
}
