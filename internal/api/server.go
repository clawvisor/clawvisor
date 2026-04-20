package api

import (
	"archive/zip"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/feedback"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/internal/mcp"
	mcpoauth "github.com/clawvisor/clawvisor/internal/mcp/oauth"
	"github.com/clawvisor/clawvisor/internal/notify/push"
	"github.com/clawvisor/clawvisor/internal/ratelimit"
	"github.com/clawvisor/clawvisor/internal/relay"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/internal/transcript"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/clawvisor/clawvisor/pkg/version"
	skillfiles "github.com/clawvisor/clawvisor/skills"
	webfs "github.com/clawvisor/clawvisor/web"

	"golang.org/x/time/rate"
)

// Server is the Clawvisor HTTP server.
type Server struct {
	cfg        *config.Config
	store      store.Store
	vault      vault.Vault
	jwtSvc     pkgauth.TokenService
	adapterReg *adapters.Registry
	notifier   notify.Notifier
	llmCfg     config.LLMConfig
	llmHealth  *llm.Health
	logger     *slog.Logger
	http       *http.Server

	magicStore pkgauth.MagicTokenStore

	// Extension points for open-core customization.
	extraRoutes           func(*http.ServeMux, Dependencies)
	wrapRoutes            func(http.Handler) http.Handler
	features              FeatureSet
	skipBuiltinAuthRoutes bool
	quiet                 bool // suppress user-facing messages (e.g. during daemon setup)

	// Relay/E2E keys.
	x25519Key     *ecdh.PrivateKey // X25519 key for E2E encryption of gateway requests
	daemonID      string           // relay daemon ID
	ed25519PubB64 string           // base64-encoded Ed25519 public key for .well-known

	// Handler references for background goroutines and decision dispatch.
	approvalsHandler   *handlers.ApprovalsHandler
	tasksHandler       *handlers.TasksHandler
	connectionsHandler *handlers.ConnectionsHandler
	devicesHandler     *handlers.DevicesHandler

	pushNotifier         *push.Notifier                 // concrete push notifier; may be nil
	msgBuffer            groupchat.Buffer               // group chat message buffer; may be nil
	decisionBus          notify.DecisionBus             // cross-instance decision delivery; may be nil
	gatewayHooks         *GatewayHooks                  // cloud-injected gateway authorization hooks; may be nil
	feedbackHooks        *FeedbackHooks                 // cloud-injected feedback event hooks; may be nil
	localServiceProvider handlers.LocalServiceProvider  // cloud-injected local daemon service provider; may be nil
	localServiceExecutor handlers.LocalServiceExecutor  // cloud-injected local service executor; may be nil

	eventHub    events.EventHub
	mcpServer   *mcp.Server
	ticketStore intauth.TicketStorer

	// Multi-instance stores (set via options; nil = use defaults).
	replayCache        middleware.ReplayCache
	tokenCache         handlers.TokenCache
	devicePairingStore handlers.DevicePairingStore
	oauthStateStore    handlers.OAuthStateStore
	pairingCodeStore   handlers.PairingCodeStore
	dedupCache         handlers.DedupCache
	verdictCache       intent.VerdictCacher

	adapterGenFactory handlers.GeneratorFactory // per-request Generator factory; set via option
}

// Dependencies is passed to ExtraRoutes so extension handlers can access shared services.
type Dependencies struct {
	Store      store.Store
	Vault      vault.Vault
	JWTService pkgauth.TokenService
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
	AdapterGen        bool `json:"adapter_gen"`
	Billing           bool `json:"billing"`
	LocalDaemon       bool `json:"local_daemon"`
	MobilePairing     bool `json:"mobile_pairing"`
}

// GatewayHooks allows cloud/enterprise layers to inject additional
// authorization logic into the gateway request flow.
type GatewayHooks struct {
	// BeforeAuthorize is called after request parsing, before restriction checks.
	// The agent (including OrgID) is available via middleware.AgentFromContext(ctx).
	// Return a non-nil error to block the request (treated as an org policy block).
	BeforeAuthorize func(ctx context.Context, agentID, userID, service, action string) error
}

// FeedbackHooks allows cloud/enterprise layers to react to feedback events.
type FeedbackHooks struct {
	// AfterBugReport is called after a bug report is successfully saved.
	AfterBugReport func(report *store.FeedbackReport)
}

// ServerOption configures optional behavior on the Server.
type ServerOption func(*Server)

// WithExtraRoutes registers additional HTTP routes (e.g. cloud-only endpoints).
func WithExtraRoutes(fn func(*http.ServeMux, Dependencies)) ServerOption {
	return func(s *Server) { s.extraRoutes = fn }
}

// WithWrapRoutes wraps the entire HTTP handler (e.g. tenant-scoping middleware).
func WithWrapRoutes(fn func(http.Handler) http.Handler) ServerOption {
	return func(s *Server) { s.wrapRoutes = fn }
}

// WithFeatures declares which capabilities the frontend should expose.
func WithFeatures(f FeatureSet) ServerOption {
	return func(s *Server) { s.features = f }
}

// WithSkipBuiltinAuth prevents the core server from registering its built-in
// login/register/password routes, allowing ExtraRoutes to provide custom auth.
func WithSkipBuiltinAuth() ServerOption {
	return func(s *Server) { s.skipBuiltinAuthRoutes = true }
}

// WithQuiet suppresses user-facing messages (startup banner, shutdown notice)
// and sets log level to WARN. Used during daemon setup phases where a temporary
// server runs in the background.
func WithQuiet() ServerOption {
	return func(s *Server) { s.quiet = true }
}

// WithE2EKey sets the X25519 private key for E2E encryption of gateway requests.
func WithE2EKey(key *ecdh.PrivateKey) ServerOption {
	return func(s *Server) { s.x25519Key = key }
}

// WithDaemonKeys sets the daemon identity keys for the .well-known/clawvisor-keys endpoint.
func WithDaemonKeys(daemonID string, x25519Key *ecdh.PrivateKey) ServerOption {
	return func(s *Server) {
		s.daemonID = daemonID
		if x25519Key != nil {
			s.x25519Key = x25519Key
		}
	}
}

// WithPushNotifier passes the concrete push notifier so the device handler
// can register/deregister device tokens and emit action decisions.
func WithPushNotifier(pn *push.Notifier) ServerOption {
	return func(s *Server) { s.pushNotifier = pn }
}

// WithGroupChatBuffer sets the message buffer for Telegram group chat
// observation. When set, task creation checks recent messages for user
// approval via LLM analysis.
func WithGroupChatBuffer(buf groupchat.Buffer) ServerOption {
	return func(s *Server) { s.msgBuffer = buf }
}

// WithEventHub overrides the default in-memory event hub with an external
// implementation (e.g. Redis-backed) for multi-instance deployments.
func WithEventHub(hub events.EventHub) ServerOption {
	return func(s *Server) { s.eventHub = hub }
}

// WithDecisionBus sets the cross-instance decision bus for callback decisions.
func WithDecisionBus(bus notify.DecisionBus) ServerOption {
	return func(s *Server) { s.decisionBus = bus }
}

// WithAdapterGenFactory sets the per-request Generator factory for adapter generation.
// The factory receives the authenticated user's ID so it can scope storage per-user
// in multi-tenant cloud deployments.
func WithAdapterGenFactory(f handlers.GeneratorFactory) ServerOption {
	return func(s *Server) { s.adapterGenFactory = f }
}

// WithGatewayHooks injects additional authorization logic into the gateway.
func WithGatewayHooks(hooks *GatewayHooks) ServerOption {
	return func(s *Server) { s.gatewayHooks = hooks }
}

// WithFeedbackHooks injects callbacks for feedback events (e.g. bug reports).
func WithFeedbackHooks(hooks *FeedbackHooks) ServerOption {
	return func(s *Server) { s.feedbackHooks = hooks }
}

// WithLocalServiceProvider injects a provider of local daemon services into
// the skill catalog.
func WithLocalServiceProvider(p handlers.LocalServiceProvider) ServerOption {
	return func(s *Server) { s.localServiceProvider = p }
}

// WithLocalServiceExecutor injects a local daemon service executor into
// the gateway for routing agent requests to connected daemons.
func WithLocalServiceExecutor(e handlers.LocalServiceExecutor) ServerOption {
	return func(s *Server) { s.localServiceExecutor = e }
}

// WithTicketStore overrides the default in-memory SSE ticket store.
func WithTicketStore(ts intauth.TicketStorer) ServerOption {
	return func(s *Server) { s.ticketStore = ts }
}

// WithReplayCache overrides the default in-memory HMAC replay cache.
func WithReplayCache(rc middleware.ReplayCache) ServerOption {
	return func(s *Server) { s.replayCache = rc }
}

// WithTokenCache overrides the default in-memory connection token cache.
func WithTokenCache(tc handlers.TokenCache) ServerOption {
	return func(s *Server) { s.tokenCache = tc }
}

// WithDevicePairingStore overrides the default in-memory device pairing store.
func WithDevicePairingStore(ps handlers.DevicePairingStore) ServerOption {
	return func(s *Server) { s.devicePairingStore = ps }
}

// WithOAuthStateStore overrides the default in-memory OAuth state store.
func WithOAuthStateStore(os handlers.OAuthStateStore) ServerOption {
	return func(s *Server) { s.oauthStateStore = os }
}

// WithPairingCodeStore overrides the default in-memory MCP pairing code store.
func WithPairingCodeStore(ps handlers.PairingCodeStore) ServerOption {
	return func(s *Server) { s.pairingCodeStore = ps }
}

// WithDedupCache overrides the default in-memory content dedup cache.
func WithDedupCache(dc handlers.DedupCache) ServerOption {
	return func(s *Server) { s.dedupCache = dc }
}

// WithVerdictCache overrides the default in-memory intent verdict cache.
func WithVerdictCache(vc intent.VerdictCacher) ServerOption {
	return func(s *Server) { s.verdictCache = vc }
}

// New creates a Server and registers all routes.
// magicStore may be nil when magic link auth is not enabled.
func New(
	cfg *config.Config,
	st store.Store,
	v vault.Vault,
	jwtSvc pkgauth.TokenService,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	llmCfg config.LLMConfig,
	magicStore pkgauth.MagicTokenStore,
	opts ...ServerOption,
) (*Server, error) {
	logOpts := &slog.HandlerOptions{Level: cfg.Server.SlogLevel()}
	var logHandler slog.Handler
	switch {
	case cfg.Server.LogFormat == "json":
		logHandler = slog.NewJSONHandler(os.Stdout, logOpts)
	case cfg.Server.LogFormat == "text":
		logHandler = slog.NewTextHandler(os.Stdout, logOpts)
	case !cfg.Server.IsLocal():
		logHandler = slog.NewJSONHandler(os.Stdout, logOpts)
	default:
		logHandler = slog.NewTextHandler(os.Stdout, logOpts)
	}
	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	s := &Server{
		cfg:        cfg,
		store:      st,
		vault:      v,
		jwtSvc:     jwtSvc,
		adapterReg: adapterReg,
		notifier:   notifier,
		llmCfg:     llmCfg,
		llmHealth:  llm.NewHealth(llmCfg),
		magicStore: magicStore,
		logger:     logger,
		eventHub:   events.NewHub(),
	}

	// Apply optional configuration.
	for _, o := range opts {
		o(s)
	}

	if s.quiet {
		s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	mux := s.routes()

	s.http = &http.Server{
		Addr:         cfg.Server.Addr(),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s, nil
}

// routes builds the HTTP mux with all registered handlers.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Public URL for links in notifications and OAuth redirects.
	// Falls back to a URL derived from the bind address.
	baseURL := s.cfg.Server.PublicURL
	if baseURL == "" {
		baseHost := s.cfg.Server.Host
		if baseHost == "0.0.0.0" || baseHost == "127.0.0.1" || baseHost == "" {
			baseHost = "localhost"
		}
		baseURL = fmt.Sprintf("http://%s:%d", baseHost, s.cfg.Server.Port)
	}

	// Handlers
	authHandler := handlers.NewAuthHandler(s.jwtSvc, s.store, s.cfg.Auth, s.magicStore, baseURL, s.cfg.Server.IsLocal())
	authMode := "magic_link"
	if s.features.Passkeys {
		authMode = "passkey"
	} else if s.features.PasswordAuth {
		authMode = "password"
	}
	healthHandler := handlers.NewHealthHandler(s.store, s.vault, authMode)
	restrictionsHandler := handlers.NewRestrictionsHandler(s.store)
	agentsHandler := handlers.NewAgentsHandler(s.store, s.eventHub, s.logger)
	auditHandler := handlers.NewAuditHandler(s.store)
	// The Telegram notifier also implements TelegramPairer and GroupObserver for
	// pairing and group chat observation flows.
	var pairer notify.TelegramPairer
	if p, ok := s.notifier.(notify.TelegramPairer); ok {
		pairer = p
	}
	var groupObs notify.GroupObserver
	if g, ok := s.notifier.(notify.GroupObserver); ok {
		groupObs = g
	}
	var groupDetector notify.GroupDetector
	if gd, ok := s.notifier.(notify.GroupDetector); ok {
		groupDetector = gd
	}
	var agentPairer notify.AgentGroupPairer
	if ap, ok := s.notifier.(notify.AgentGroupPairer); ok {
		agentPairer = ap
	}
	var groupValidator notify.GroupMembershipValidator
	if gv, ok := s.notifier.(notify.GroupMembershipValidator); ok {
		groupValidator = gv
	}
	notificationsHandler := handlers.NewNotificationsHandler(s.store, s.notifier, pairer, groupObs, groupDetector, agentPairer, groupValidator, baseURL)
	// Construct intent verifier (noop if disabled).
	var verifier intent.Verifier = intent.NoopVerifier{}
	if s.llmCfg.Verification.Enabled {
		v := intent.NewLLMVerifier(s.llmHealth, s.logger)
		if s.verdictCache != nil {
			v.SetVerdictCache(s.verdictCache)
		}
		verifier = v
	}

	// Construct chain context extractor (noop if disabled).
	var extractor intent.Extractor = intent.NoopExtractor{}
	if s.llmCfg.ChainContext.Enabled {
		extractor = intent.NewLLMExtractor(s.llmHealth, s.logger)
	}

	gatewayHandler := handlers.NewGatewayHandler(
		s.store, s.vault, s.adapterReg,
		s.notifier, verifier, extractor, *s.cfg, s.logger, baseURL, s.eventHub,
	)
	if s.gatewayHooks != nil {
		gatewayHandler.SetGatewayHooks(&handlers.GatewayHooks{
			BeforeAuthorize: s.gatewayHooks.BeforeAuthorize,
		})
	}
	if s.localServiceExecutor != nil {
		gatewayHandler.SetLocalServiceExecutor(s.localServiceExecutor)
	}
	if s.localServiceProvider != nil {
		gatewayHandler.SetLocalServiceProvider(s.localServiceProvider)
	}
	servicesHandler := handlers.NewServicesHandler(s.store, s.vault, s.adapterReg, s.logger, baseURL, s.eventHub)
	if s.oauthStateStore != nil {
		servicesHandler.SetOAuthStateStore(s.oauthStateStore)
	}
	// Set relay daemon URL for PKCE flows that require HTTPS redirect URIs.
	if s.cfg.Relay.Enabled && s.cfg.Relay.URL != "" && s.cfg.Relay.DaemonID != "" {
		relayHost := strings.TrimPrefix(strings.TrimPrefix(s.cfg.Relay.URL, "wss://"), "ws://")
		servicesHandler.SetRelayDaemonURL(fmt.Sprintf("https://%s/d/%s", relayHost, s.cfg.Relay.DaemonID))
	}
	skillHandler := handlers.NewSkillHandler(s.store, s.vault, s.adapterReg, s.logger)
	if s.localServiceProvider != nil {
		skillHandler.SetLocalServiceProvider(s.localServiceProvider)
	}
	approvalsHandler := handlers.NewApprovalsHandler(s.store, s.vault, s.adapterReg, s.notifier, s.logger, s.eventHub)
	s.approvalsHandler = approvalsHandler

	// Construct task risk assessor (noop if disabled).
	var assessor taskrisk.Assessor = taskrisk.NoopAssessor{}
	if s.llmCfg.TaskRisk.Enabled {
		assessor = taskrisk.NewLLMAssessor(s.llmHealth, s.adapterReg, s.logger)
	}

	tasksHandler := handlers.NewTasksHandler(s.store, s.vault, s.adapterReg,
		s.notifier, *s.cfg, s.logger, baseURL, s.eventHub, assessor)
	if s.dedupCache != nil {
		tasksHandler.SetDedupCache(s.dedupCache)
	}
	if s.msgBuffer != nil {
		tasksHandler.SetGroupApproval(s.msgBuffer, s.llmHealth, agentPairer)
	}
	if s.localServiceProvider != nil {
		tasksHandler.SetLocalServiceProvider(s.localServiceProvider)
	}
	s.tasksHandler = tasksHandler
	if s.ticketStore == nil {
		s.ticketStore = intauth.NewTicketStore()
	}
	eventsHandler := handlers.NewEventsHandler(s.eventHub, s.ticketStore)

	// Middleware
	requireUser := middleware.RequireUser(s.jwtSvc, s.store)
	requireAgent := middleware.RequireAgent(s.store)
	requireBridge := middleware.RequireBridge(s.store)
	requireProxy := middleware.RequireProxy(s.store)
	logMiddleware := middleware.Logging(s.logger)
	recoverMiddleware := middleware.Recover(s.logger)
	securityMiddleware := middleware.Security(s.cfg.Server.IsLocal() && s.cfg.Server.PublicURL == "")

	// Rate limiters (skip when config is zero-valued, e.g. in tests)
	rlCfg := s.cfg.RateLimit
	gatewayRL := newKeyedLimiterFromBucket(rlCfg.Gateway)
	oauthRL := newKeyedLimiterFromBucket(rlCfg.OAuth)
	policyRL := newKeyedLimiterFromBucket(rlCfg.PolicyAPI)
	authRL := newKeyedLimiterFromBucket(rlCfg.Auth)

	ipKeyFn := func(r *http.Request) string {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
	agentKeyFn := func(r *http.Request) string {
		if a := middleware.AgentFromContext(r.Context()); a != nil {
			return a.ID
		}
		return ""
	}
	userKeyFn := func(r *http.Request) string {
		if u := middleware.UserFromContext(r.Context()); u != nil {
			return u.ID
		}
		return ""
	}

	user := func(h http.HandlerFunc) http.Handler { return requireUser(h) }
	userOAuthRL := func(h http.HandlerFunc) http.Handler {
		return requireUser(middleware.RateLimit(oauthRL, userKeyFn, rlCfg.OAuth.Limit)(h))
	}
	userPolicyRL := func(h http.HandlerFunc) http.Handler {
		return requireUser(middleware.RateLimit(policyRL, userKeyFn, rlCfg.PolicyAPI.Limit)(h))
	}
	authRateLimited := func(h http.HandlerFunc) http.Handler {
		return middleware.RateLimit(authRL, ipKeyFn, rlCfg.Auth.Limit)(h)
	}

	// Health (no auth)
	mux.HandleFunc("GET /health", healthHandler.Health)
	mux.HandleFunc("GET /ready", healthHandler.Ready)
	mux.HandleFunc("GET /api/config/public", healthHandler.ConfigPublic)
	mux.HandleFunc("GET /api/version", healthHandler.Version)
	mux.HandleFunc("GET /api/skill/version", healthHandler.SkillVersion)

	// LLM status and runtime config update
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}
	llmHandler := handlers.NewLLMHandler(s.llmHealth, configPath)
	mux.Handle("GET /api/llm/status", user(llmHandler.Status))
	// Cloud (multi-tenant) deployments manage LLM config centrally;
	// only self-hosted installs may update it through the dashboard.
	if !s.features.MultiTenant {
		mux.Handle("PUT /api/llm", user(llmHandler.Update))
	}

	// Auth — core routes (always registered)
	mux.Handle("POST /api/auth/refresh", authRateLimited(authHandler.Refresh))
	mux.Handle("POST /api/auth/logout", user(authHandler.Logout))
	mux.Handle("GET /api/me", user(authHandler.Me))

	// Magic link auth — /local is always registered so the CLI gets a
	// proper JSON error instead of the SPA HTML when magic links are disabled.
	mux.Handle("POST /api/auth/magic/local", authRateLimited(authHandler.GenerateMagicLocal))
	if s.magicStore != nil {
		mux.Handle("POST /api/auth/magic", authRateLimited(authHandler.ExchangeMagic))
	}

	// Password auth routes are registered only when the PasswordAuth feature is enabled
	// AND the cloud layer hasn't opted to provide its own auth routes.
	// In the open-source build this is off by default (local mode uses magic links).
	// Cloud and self-hosted password deployments enable it via WithFeatures.
	if s.features.PasswordAuth && !s.skipBuiltinAuthRoutes {
		mux.Handle("POST /api/auth/register", authRateLimited(authHandler.Register))
		mux.Handle("POST /api/auth/login", authRateLimited(authHandler.Login))
		mux.Handle("PUT /api/me", user(authHandler.UpdateMe))
		mux.Handle("DELETE /api/me", user(authHandler.DeleteMe))
	}

	// Features endpoint (always registered, returns the active FeatureSet)
	mux.HandleFunc("GET /api/features", s.handleFeatures)

	// Restrictions (rate-limited writes)
	mux.Handle("GET /api/restrictions", user(restrictionsHandler.List))
	mux.Handle("POST /api/restrictions", userPolicyRL(restrictionsHandler.Create))
	mux.Handle("DELETE /api/restrictions/{id}", userPolicyRL(restrictionsHandler.Delete))

	// Agents (user JWT)
	mux.Handle("GET /api/agents", user(agentsHandler.List))
	mux.Handle("POST /api/agents", user(agentsHandler.Create))
	mux.Handle("POST /api/agents/{id}/rotate", user(agentsHandler.RotateToken))
	mux.Handle("DELETE /api/agents/{id}", user(agentsHandler.Delete))

	// Notifications (user JWT)
	mux.Handle("GET /api/notifications", user(notificationsHandler.List))
	mux.Handle("PUT /api/notifications/telegram", user(notificationsHandler.UpsertTelegram))
	mux.Handle("DELETE /api/notifications/telegram", user(notificationsHandler.DeleteTelegram))
	mux.Handle("POST /api/notifications/telegram/test", user(notificationsHandler.TestTelegram))
	mux.Handle("POST /api/notifications/telegram/pair", user(notificationsHandler.StartPairing))
	mux.Handle("GET /api/notifications/telegram/pair/{pairing_id}", user(notificationsHandler.PairingStatus))
	mux.Handle("POST /api/notifications/telegram/pair/{pairing_id}/confirm", user(notificationsHandler.ConfirmPairing))
	mux.Handle("POST /api/notifications/telegram/group", user(notificationsHandler.UpsertTelegramGroup))
	mux.Handle("POST /api/notifications/telegram/groups/detect", user(notificationsHandler.DetectTelegramGroups))
	mux.Handle("GET /api/notifications/telegram/groups", user(notificationsHandler.ListTelegramGroups))
	mux.Handle("DELETE /api/notifications/telegram/groups/{chat_id}", user(notificationsHandler.DismissTelegramGroup))
	// Multi-group management
	mux.Handle("POST /api/notifications/telegram/groups/manual", user(notificationsHandler.AddGroupManually))
	mux.Handle("GET /api/notifications/telegram/groups/active", user(notificationsHandler.ListActiveGroups))
	mux.Handle("DELETE /api/notifications/telegram/groups/active/{group_chat_id}", user(notificationsHandler.DeleteTelegramGroup))
	mux.Handle("PUT /api/notifications/telegram/groups/active/{group_chat_id}/auto-approval", user(notificationsHandler.SetAutoApproval))
	mux.Handle("POST /api/notifications/telegram/groups/active/{group_chat_id}/pair", user(notificationsHandler.CreateGroupPairing))
	mux.Handle("GET /api/notifications/telegram/groups/active/{group_chat_id}/agents", user(notificationsHandler.ListPairedAgents))
	mux.Handle("POST /api/notifications/telegram/groups/pair/{session_id}", requireAgent(http.HandlerFunc(notificationsHandler.PairAgentToGroup)))

	// E2E encryption middleware — wraps agent-facing routes so relay traffic is encrypted.
	// Local requests pass through unencrypted; relay requests without E2E get 403.
	e2e := func(h http.Handler) http.Handler { return h }
	if s.x25519Key != nil {
		e2eMw := middleware.E2E(s.x25519Key)
		e2e = func(h http.Handler) http.Handler { return e2eMw(h) }
	}

	// Connection requests (unauthenticated — agents requesting access)
	connectionsHandler := handlers.NewConnectionsHandler(s.store, s.notifier, s.eventHub, s.logger, baseURL, s.features.MultiTenant)
	if s.tokenCache != nil {
		connectionsHandler.SetTokenCache(s.tokenCache)
	}
	s.connectionsHandler = connectionsHandler
	connectionsRL := newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 10, Window: 60})
	mux.Handle("POST /api/agents/connect",
		middleware.RateLimit(connectionsRL, ipKeyFn, 10)(e2e(http.HandlerFunc(connectionsHandler.RequestConnect))))
	mux.Handle("GET /api/agents/connect/{id}/status", e2e(http.HandlerFunc(connectionsHandler.PollStatus)))

	// Connection request management (user JWT)
	mux.Handle("GET /api/agents/connections", user(connectionsHandler.List))
	mux.Handle("POST /api/agents/connect/{id}/approve", user(connectionsHandler.Approve))
	mux.Handle("POST /api/agents/connect/{id}/deny", user(connectionsHandler.Deny))

	// Pairing code (for relay MCP OAuth consent — no auth, CORS for relay origin)
	var pairingHandler *handlers.PairingHandler
	if s.daemonID != "" {
		pairingHandler = handlers.NewPairingHandler(s.daemonID)
		if s.pairingCodeStore != nil {
			pairingHandler.SetPairingCodeStore(s.pairingCodeStore)
		}
		corsOrigins := version.CORSOrigins()
		mux.Handle("GET /api/pairing/code",
			middleware.CORSAllowOrigins(corsOrigins, http.HandlerFunc(pairingHandler.GenerateCode)))
		mux.Handle("OPTIONS /api/pairing/code",
			middleware.CORSAllowOrigins(corsOrigins, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	}

	// Device pairing and management
	devicesHandler := handlers.NewDevicesHandler(s.store, s.pushNotifier, s.eventHub, s.logger, baseURL, s.jwtSvc)
	if s.devicePairingStore != nil {
		devicesHandler.SetPairingStore(s.devicePairingStore)
	}
	if s.daemonID != "" {
		relayHost := relayHostFromCfg(s.cfg.Relay.URL)
		devicesHandler.SetRelayInfo(s.daemonID, relayHost)
	}
	s.devicesHandler = devicesHandler
	var requireDevice func(http.Handler) http.Handler
	if s.replayCache != nil {
		requireDevice = middleware.RequireDeviceWithReplayCache(s.store, s.replayCache)
	} else {
		requireDevice = middleware.RequireDevice(s.store)
	}
	// Device-to-server routes (always available for already-paired devices)
	mux.Handle("POST /api/devices/{id}/action", requireDevice(e2e(http.HandlerFunc(devicesHandler.Action))))
	mux.Handle("POST /api/devices/{id}/token", requireDevice(e2e(http.HandlerFunc(devicesHandler.MintToken))))
	mux.Handle("POST /api/devices/{id}/push-to-start-token", requireDevice(e2e(http.HandlerFunc(devicesHandler.UpdatePushToStartToken))))
	// Pairing and management routes (gated by mobile_pairing feature flag)
	if s.features.MobilePairing {
		devicesRL := newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 5, Window: 60})
		mux.Handle("GET /api/devices/pair/info", user(devicesHandler.PairInfo))
		mux.Handle("POST /api/devices/pair", user(devicesHandler.StartPairing))
		mux.Handle("POST /api/devices/pair/complete",
			middleware.RateLimit(devicesRL, ipKeyFn, 5)(e2e(http.HandlerFunc(devicesHandler.CompletePairing))))
		mux.Handle("GET /api/devices", user(devicesHandler.List))
		mux.Handle("DELETE /api/devices/{id}", user(devicesHandler.Delete))
	}

	// Guard (agent token — Claude Code permission check)
	guardHandler := handlers.NewGuardHandler(s.store, verifier, s.adapterReg, s.logger)
	mux.Handle("POST /api/guard/check", requireAgent(e2e(http.HandlerFunc(guardHandler.Check))))

	// Gateway (agent token, rate-limited, E2E on relay traffic)
	mux.Handle("POST /api/gateway/request", requireAgent(middleware.RateLimit(gatewayRL, agentKeyFn, rlCfg.Gateway.Limit)(
		e2e(http.HandlerFunc(gatewayHandler.HandleRequest)))))
	mux.Handle("GET /api/gateway/request/{request_id}", requireAgent(middleware.RateLimit(gatewayRL, agentKeyFn, rlCfg.Gateway.Limit)(
		e2e(http.HandlerFunc(gatewayHandler.HandleGet)))))
	mux.Handle("POST /api/gateway/request/{request_id}/execute", requireAgent(middleware.RateLimit(gatewayRL, agentKeyFn, rlCfg.Gateway.Limit)(
		e2e(http.HandlerFunc(gatewayHandler.HandleExecuteApproved)))))

	// Adapter generation (user JWT for dashboard, agent token for MCP)
	if s.llmCfg.AdapterGen.Enabled && s.adapterGenFactory != nil {
		s.features.AdapterGen = true
		adapterGenHandler := handlers.NewAdapterGenHandler(s.adapterGenFactory, s.logger)
		mux.Handle("POST /api/adapters/generate", user(adapterGenHandler.Create))
		mux.Handle("POST /api/adapters/install", user(adapterGenHandler.Install))
		mux.Handle("PUT /api/adapters/{service_id}/generate", user(adapterGenHandler.Update))
		mux.Handle("DELETE /api/adapters/{service_id}", user(adapterGenHandler.Remove))
	}

	// Agent feedback (agent token for reporting, user JWT for listing)
	var feedbackReviewer feedback.Reviewer = feedback.NoopReviewer{}
	if s.llmCfg.FeedbackReview.Enabled {
		feedbackReviewer = feedback.NewLLMReviewer(s.llmHealth, s.logger)
	}
	feedbackHandler := handlers.NewFeedbackHandler(s.store, feedbackReviewer, s.logger)
	if s.feedbackHooks != nil && s.feedbackHooks.AfterBugReport != nil {
		feedbackHandler.SetAfterBugReport(s.feedbackHooks.AfterBugReport)
	}
	mux.Handle("POST /api/feedback/report", requireAgent(e2e(http.HandlerFunc(feedbackHandler.ReportBug))))
	mux.Handle("POST /api/feedback/nps", requireAgent(e2e(http.HandlerFunc(feedbackHandler.SubmitNPS))))
	mux.Handle("GET /api/feedback/reports", user(feedbackHandler.ListReports))

	// Callback secret registration (agent token)
	mux.Handle("POST /api/callbacks/register", requireAgent(e2e(http.HandlerFunc(gatewayHandler.RegisterCallback))))

	// Buffer ingest (bridge token) — lets a paired OpenClaw plugin forward
	// conversation messages so auto-approval has the full chat context even
	// when Clawvisor does not own messaging I/O. Authenticated via a bridge
	// token (cvisbr_...) — never an agent token — so the agent (which is
	// untrusted LLM output) can't plant approval messages.
	if s.msgBuffer != nil {
		bufferHandler := handlers.NewBufferHandler(s.msgBuffer, s.store, s.logger)
		mux.Handle("POST /api/buffer/ingest", requireBridge(e2e(http.HandlerFunc(bufferHandler.Ingest))))
	}

	// Plugin pairing (OpenClaw). The plugin calls POST /api/plugin/pair with
	// a user-id embedded from /skill/setup-openclaw, polls for approval,
	// and receives a bridge token plus per-agent tokens on success. Only
	// user-JWT endpoints (approve/deny/list/patch/revoke) require auth;
	// the request + poll endpoints are unauthenticated (like agent connect).
	pluginPairing := handlers.NewPluginPairingHandler(s.store, s.eventHub, s.logger)
	pluginPairing.SetServerVersion(version.Version)
	// Tell the pair handler how the proxy container should reach the server
	// from inside its docker network. For the default self-hosted
	// deployment, the proxy container lives alongside the server and
	// reaches it via `host.docker.internal` (the same path the OpenClaw
	// container already uses at Stage 0). An operator running a bespoke
	// compose setup can override via CLAWVISOR_SERVER_URL_FOR_PROXY in the
	// install artifact's docker-compose template.
	proxyServerURL := s.cfg.Server.PublicURL
	if proxyServerURL == "" {
		proxyServerURL = "http://host.docker.internal:25297"
	}
	pluginPairing.SetServerURL(proxyServerURL)
	if s.msgBuffer != nil {
		pluginPairing.SetBuffer(s.msgBuffer)
	}
	mux.HandleFunc("POST /api/plugin/pair", pluginPairing.RequestPair)
	mux.HandleFunc("GET /api/plugin/pair/{id}", pluginPairing.PollPair)
	mux.Handle("POST /api/plugin/pair/{id}/approve", user(pluginPairing.ApprovePair))
	mux.Handle("POST /api/plugin/pair/{id}/deny", user(pluginPairing.DenyPair))
	mux.Handle("GET /api/plugin/pair/pending", user(pluginPairing.ListPendingPairs))
	mux.Handle("POST /api/plugin/pair-codes", user(pluginPairing.MintPairCode))
	mux.Handle("POST /api/plugin/agents", requireBridge(e2e(http.HandlerFunc(pluginPairing.RequestAgentAdd))))
	mux.Handle("GET /api/plugin/bridges", user(pluginPairing.ListBridges))
	mux.Handle("GET /api/plugin/bridges/{id}/buffer", user(pluginPairing.BufferForBridge))
	mux.Handle("PATCH /api/plugin/bridges/{id}", user(pluginPairing.PatchBridge))
	mux.Handle("DELETE /api/plugin/bridges/{id}", user(pluginPairing.RevokeBridge))
	// Stage 1 proxy enablement: dashboard-driven opt-in + install artifact.
	// See docs/design-proxy-stage1.md §3.2 + §8.3.
	mux.Handle("POST /api/plugin/bridges/{id}/enable-proxy", user(pluginPairing.EnableProxy))
	mux.Handle("POST /api/plugin/bridges/{id}/disable-proxy", user(pluginPairing.DisableProxy))
	mux.Handle("GET /api/plugin/bridges/{id}/install-artifact", user(pluginPairing.InstallArtifact))
	// Stage 2 M4: framework-agnostic install — create a standalone
	// proxy-only bridge (no plugin pair) and return the install artifact.
	// See docs/design-proxy-stage2.md §M4.
	mux.Handle("POST /api/plugin/bridges/proxy-only", user(pluginPairing.CreateProxyOnlyBridge))
	// Plugin-read runtime config (authed by bridge token, not user JWT).
	// Plugin hits this on startup + periodic heartbeat to decide whether
	// to run its scavenger code path.
	mux.Handle("GET /api/plugin/bridges/self/config", requireBridge(e2e(http.HandlerFunc(pluginPairing.BridgeSelfConfig))))

	// Proxy-facing endpoints (Clawvisor Proxy; cvisproxy_... bearer token).
	// See docs/proxy-api.md §5 for the wire contract. The proxy authenticates
	// once and calls these to fetch config, ingest TurnEvents, and rotate
	// signing keys. Never reached by agents or plugins.
	proxyHandler := handlers.NewProxyHandler(s.store, s.logger)
	mux.Handle("GET /api/proxy/config", requireProxy(e2e(http.HandlerFunc(proxyHandler.Config))))
	mux.Handle("POST /api/proxy/turns", requireProxy(e2e(http.HandlerFunc(proxyHandler.Turns))))
	mux.Handle("POST /api/proxy/signing-keys/rotate", requireProxy(e2e(http.HandlerFunc(proxyHandler.SigningKeyRotate))))

	// Stage 2: credential injection lookup (proxy-scoped) +
	// user-facing vault UX for injectable credentials.
	// See docs/design-proxy-stage2.md §2.
	credHandler := handlers.NewCredentialHandler(s.store, s.vault, s.logger)
	mux.Handle("POST /api/proxy/credential-lookup", requireProxy(e2e(http.HandlerFunc(credHandler.Lookup))))
	mux.Handle("POST /api/vault/injectable-credentials", user(credHandler.UpsertCredential))
	mux.Handle("GET /api/vault/injectable-credentials", user(credHandler.ListCredentials))
	mux.Handle("GET /api/vault/injectable-credentials/usage", user(credHandler.UsageLog))
	mux.Handle("DELETE /api/vault/injectable-credentials/{ref}", user(credHandler.RevokeCredential))
	// Seed built-in injection rules on every boot (idempotent).
	go handlers.SeedBuiltInInjectionRules(context.Background(), s.store, s.logger)

	// Services / OAuth (user JWT, rate-limited)
	mux.Handle("GET /api/services", user(servicesHandler.List))
	mux.Handle("GET /api/oauth/url", userOAuthRL(servicesHandler.OAuthGetURL))  // fetch → returns {"url":"..."}
	mux.Handle("GET /api/oauth/start", userOAuthRL(servicesHandler.OAuthStart)) // kept for compat
	mux.HandleFunc("GET /api/oauth/callback", servicesHandler.OAuthCallback)    // no auth: browser redirect
	mux.Handle("POST /api/services/{serviceID}/activate", user(servicesHandler.Activate))
	mux.Handle("POST /api/services/{serviceID}/activate-key", user(servicesHandler.ActivateWithKey))
	mux.Handle("POST /api/services/{serviceID}/deactivate", user(servicesHandler.Deactivate))
	mux.Handle("POST /api/services/{serviceID}/rename-alias", user(servicesHandler.RenameAlias))
	mux.Handle("POST /api/services/{serviceID}/device-flow/start", user(servicesHandler.DeviceFlowStart))
	mux.Handle("POST /api/services/{serviceID}/device-flow/poll", user(servicesHandler.DeviceFlowPoll))
	mux.Handle("POST /api/services/{serviceID}/pkce-flow/start", user(servicesHandler.PKCEFlowStart))
	mux.HandleFunc("GET /api/pkce-flow/callback", servicesHandler.PKCEFlowCallback) // no auth: browser redirect

	// System-level OAuth config (user JWT)
	mux.Handle("GET /api/system/google-oauth", user(servicesHandler.GetGoogleOAuthConfig))
	mux.Handle("POST /api/system/google-oauth", user(servicesHandler.SetGoogleOAuthConfig))
	mux.Handle("GET /api/system/pkce-credentials", user(servicesHandler.ListPKCECredentials))
	mux.Handle("POST /api/system/pkce-credentials", user(servicesHandler.SetPKCECredential))
	mux.Handle("DELETE /api/system/pkce-credentials/{service_id}", user(servicesHandler.DeletePKCECredential))

	// Skill catalog (agent token)
	mux.Handle("GET /api/skill/catalog", requireAgent(e2e(http.HandlerFunc(skillHandler.Catalog))))

	// Approvals (user JWT)
	mux.Handle("GET /api/approvals", user(approvalsHandler.List))
	mux.Handle("POST /api/approvals/{request_id}/approve", user(approvalsHandler.Approve))
	mux.Handle("POST /api/approvals/{request_id}/deny", user(approvalsHandler.Deny))

	// Unified queue (user JWT)
	queueHandler := handlers.NewQueueHandler(s.store)
	mux.Handle("GET /api/queue", user(queueHandler.List))

	// Overview (user JWT)
	overviewHandler := handlers.NewOverviewHandler(s.store)
	mux.Handle("GET /api/overview", user(overviewHandler.Get))

	// Welcome / "What is Clawvisor?" page (user JWT)
	welcomeHandler := handlers.NewWelcomeHandler(s.store, s.vault, s.adapterReg, s.llmHealth, s.logger)
	mux.Handle("GET /api/welcome/suggestions", user(welcomeHandler.Suggestions))

	// Tasks (agent auth)
	mux.Handle("POST /api/tasks", requireAgent(e2e(http.HandlerFunc(tasksHandler.Create))))
	mux.Handle("GET /api/tasks/{id}", requireAgent(e2e(http.HandlerFunc(tasksHandler.Get))))
	mux.Handle("POST /api/tasks/{id}/complete", requireAgent(e2e(http.HandlerFunc(tasksHandler.Complete))))
	mux.Handle("POST /api/tasks/{id}/expand", requireAgent(e2e(http.HandlerFunc(tasksHandler.Expand))))

	// Tasks (user JWT)
	mux.Handle("GET /api/tasks", user(tasksHandler.List))
	mux.Handle("POST /api/tasks/{id}/approve", user(tasksHandler.Approve))
	mux.Handle("POST /api/tasks/{id}/deny", user(tasksHandler.Deny))
	mux.Handle("POST /api/tasks/{id}/revoke", user(tasksHandler.Revoke))
	mux.Handle("POST /api/tasks/{id}/expand/approve", user(tasksHandler.ExpandApprove))
	mux.Handle("POST /api/tasks/{id}/expand/deny", user(tasksHandler.ExpandDeny))

	// Audit (user JWT)
	mux.Handle("GET /api/audit", user(auditHandler.List))
	mux.Handle("GET /api/audit/{id}", user(auditHandler.Get))

	// SSE event stream (user JWT or single-use ticket for EventSource)
	requireUserOrTicket := middleware.RequireUserOrTicket(s.jwtSvc, s.store, s.ticketStore)
	mux.Handle("GET /api/events", requireUserOrTicket(http.HandlerFunc(eventsHandler.Stream)))
	mux.Handle("POST /api/events/ticket", user(eventsHandler.IssueTicket))

	// Skill files (no auth — served so OpenClaw instances can install the skill)
	// GET /skill         → redirects to /skill/SKILL.md
	// GET /skill/*       → embedded clawvisor skill tree (SKILL.md, policies/, …)
	// GET /skill/setup   → agent onboarding document with pre-filled CLAWVISOR_URL
	skillFS, _ := fs.Sub(skillfiles.FS, "clawvisor")
	skillFileHandler := http.StripPrefix("/skill", http.FileServer(http.FS(skillFS)))
	mux.HandleFunc("GET /skill", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/skill/SKILL.md", http.StatusFound)
	})
	{
		relayHost := relayHostFromCfg(s.cfg.Relay.URL)
		onboardingHandler := handlers.NewOnboardingHandler(relayHost, s.daemonID, s.cfg.Server.IsLocal())
		mux.HandleFunc("GET /skill/setup", onboardingHandler.Setup) // legacy alias → Hermes flow
		mux.HandleFunc("GET /skill/setup-hermes", onboardingHandler.SetupHermes)
		mux.HandleFunc("GET /skill/setup-openclaw", onboardingHandler.SetupOpenClaw)
		mux.HandleFunc("GET /skill/clawvisor-setup.md", onboardingHandler.ClaudeCodeSetup)

		// OpenClaw plugin tarball — served so setup-openclaw can point OpenClaw
		// at a plugin that's matched to this server's protocol version. Bytes
		// embedded at build time from extensions/clawvisor/ (see `make
		// plugin-bundle`). Users who want a version-pinned URL can read
		// /skill/openclaw-plugin.version first and include that in their
		// reference — we don't register a versioned route because it would
		// collide with the /skill/ file-server catch-all below.
		pluginBundleHandler := handlers.NewPluginBundleHandler(s.logger)
		mux.HandleFunc("GET /skill/openclaw-plugin.tgz", pluginBundleHandler.ServeTarball)
		mux.HandleFunc("GET /skill/openclaw-plugin.sha256", pluginBundleHandler.ServeSHA256)
		mux.HandleFunc("GET /skill/openclaw-plugin.version", pluginBundleHandler.ServeVersion)
	}
	// skillRenderOpts builds RenderOptions based on whether the request
	// arrived directly (local) or via the relay (cloud).
	skillRenderOpts := func(r *http.Request) skillfiles.RenderOptions {
		viaRelay := relay.ViaRelay(r.Context())
		var url string
		if viaRelay && s.daemonID != "" {
			rh := relayHostFromCfg(s.cfg.Relay.URL)
			url = fmt.Sprintf("https://%s/d/%s", rh, s.daemonID)
		} else {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			url = scheme + "://" + r.Host
		}
		return skillfiles.RenderOptions{
			ClawvisorURL:    url,
			ViaRelay:        viaRelay,
			FeedbackEnabled: s.llmCfg.FeedbackReview.Enabled,
		}
	}

	mux.HandleFunc("GET /skill/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		rendered, err := skillfiles.RenderWithOptions(skillfiles.TargetClaudeCode, skillRenderOpts(r))
		if err != nil {
			http.Error(w, "rendering SKILL.md: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte(rendered))
	})
	mux.HandleFunc("GET /skill/skill.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="skill.zip"`)
		rendered, err := skillfiles.RenderWithOptions(skillfiles.TargetClaudeCode, skillRenderOpts(r))
		if err != nil {
			http.Error(w, "rendering SKILL.md: "+err.Error(), http.StatusInternalServerError)
			return
		}
		zw := zip.NewWriter(w)
		for _, entry := range []struct{ name, content string }{
			{"SKILL.md", rendered},
		} {
			f, err := zw.Create(entry.name)
			if err != nil {
				return
			}
			f.Write([]byte(entry.content))
		}
		// e2e.mjs is a static file — read from the embedded FS.
		if data, err := fs.ReadFile(skillFS, "e2e.mjs"); err == nil {
			if f, err := zw.Create("e2e.mjs"); err == nil {
				f.Write(data)
			}
		}
		zw.Close()
	})
	mux.Handle("/skill/", skillFileHandler)

	// Key discovery endpoint (no auth — agents need the public key for E2E).
	if s.daemonID != "" && s.x25519Key != nil {
		mux.HandleFunc("GET /.well-known/clawvisor-keys", s.handleClawvisorKeys)
	}

	// MCP endpoint (agent token auth)
	if s.cfg.MCP.Enabled {
		sessionTTL := time.Duration(s.cfg.MCP.SessionTTL) * time.Minute
		if sessionTTL <= 0 {
			sessionTTL = 24 * time.Hour
		}

		// Build handler map for tool execution — each tool calls an existing handler.
		// No auth middleware here: the MCP handler already authenticates the agent
		// and injects it into the context before tool execution.
		mcpHandlers := map[string]http.Handler{
			"GET /api/skill/catalog":                         http.HandlerFunc(skillHandler.Catalog),
			"POST /api/tasks":                                http.HandlerFunc(tasksHandler.Create),
			"GET /api/tasks/{id}":                            http.HandlerFunc(tasksHandler.Get),
			"POST /api/tasks/{id}/complete":                  http.HandlerFunc(tasksHandler.Complete),
			"POST /api/tasks/{id}/expand":                    http.HandlerFunc(tasksHandler.Expand),
			"POST /api/gateway/request":                      http.HandlerFunc(gatewayHandler.HandleRequest),
			"POST /api/gateway/request/{request_id}/execute": http.HandlerFunc(gatewayHandler.HandleExecuteApproved),
		}

		// Register adapter generation routes in MCP handler map if enabled.
		if s.llmCfg.AdapterGen.Enabled && s.adapterGenFactory != nil {
			mcpAdapterGenHandler := handlers.NewAdapterGenHandler(s.adapterGenFactory, s.logger)
			mcpHandlers["POST /api/adapters/generate"] = http.HandlerFunc(mcpAdapterGenHandler.Create)
			mcpHandlers["PUT /api/adapters/{service_id}/generate"] = http.HandlerFunc(mcpAdapterGenHandler.Update)
			mcpHandlers["DELETE /api/adapters/{service_id}"] = http.HandlerFunc(mcpAdapterGenHandler.Remove)
		}

		mcpServer := mcp.NewServer(s.store, sessionTTL, mcpHandlers, s.logger)
		s.mcpServer = mcpServer

		mcpHandler := handlers.NewMCPHandler(mcpServer, s.store, baseURL)
		mux.HandleFunc("POST /mcp", mcpHandler.Handle)
		mux.HandleFunc("GET /mcp", mcpHandler.HandleSSE)
		mux.HandleFunc("DELETE /mcp", mcpHandler.HandleDelete)

		// OAuth 2.1 (for MCP clients)
		var oauthOpts []mcpoauth.ProviderOption
		if s.daemonID != "" {
			oauthOpts = append(oauthOpts, mcpoauth.WithDaemonID(s.daemonID))
		}
		if pairingHandler != nil {
			oauthOpts = append(oauthOpts, mcpoauth.WithPairingVerifier(pairingHandler.Verify))
		}
		oauthProvider := mcpoauth.NewProvider(s.store, s.jwtSvc, baseURL, s.logger, oauthOpts...)
		mux.HandleFunc("GET /.well-known/oauth-protected-resource", oauthProvider.ProtectedResourceMetadata)
		mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauthProvider.AuthorizationServerMetadata)
		// Rate limit registration to prevent database flooding.
		mux.Handle("POST /oauth/register", middleware.RateLimit(oauthRL, ipKeyFn, rlCfg.OAuth.Limit)(http.HandlerFunc(oauthProvider.Register)))
		// GET /oauth/authorize is handled by the SPA (React consent page).
		// The frontend POSTs to POST /oauth/authorize on approval.
		mux.HandleFunc("POST /oauth/authorize", oauthProvider.AuthorizeApprove)
		mux.HandleFunc("POST /oauth/deny", oauthProvider.AuthorizeDeny)
		mux.HandleFunc("POST /oauth/token", oauthProvider.Token)
	}

	// Extension hook: let cloud/enterprise layers add additional routes.
	if s.extraRoutes != nil {
		s.extraRoutes(mux, Dependencies{
			Store:      s.store,
			Vault:      s.vault,
			JWTService: s.jwtSvc,
			AdapterReg: s.adapterReg,
			Notifier:   s.notifier,
			Logger:     s.logger,
			BaseURL:    baseURL,
		})
	}

	// SPA fallback — serve from disk if configured, otherwise from embedded FS.
	if s.cfg.Server.FrontendDir != "" {
		fileServer := http.FileServer(http.Dir(s.cfg.Server.FrontendDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			path := s.cfg.Server.FrontendDir + r.URL.Path
			if _, err := os.Stat(path); os.IsNotExist(err) || r.URL.Path == "/" {
				// index.html must not be cached — it references content-hashed
				// JS/CSS chunks, so a stale copy serves outdated code.
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFile(w, r, s.cfg.Server.FrontendDir+"/index.html")
				return
			}
			fileServer.ServeHTTP(w, r)
		})
	} else if distFS, err := fs.Sub(webfs.DistFS, "dist"); err == nil {
		fileServer := http.FileServer(http.FS(distFS))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			// Check if the file exists in the embedded FS.
			if f, err := distFS.Open(strings.TrimPrefix(r.URL.Path, "/")); err == nil {
				f.Close()
				if r.URL.Path != "/" {
					fileServer.ServeHTTP(w, r)
					return
				}
			}
			w.Header().Set("Cache-Control", "no-cache")
			index, _ := fs.ReadFile(distFS, "index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(index)
		})
	}

	// Recover is innermost (after logging/security) so panics become 500s
	// with a logged stack trace, and the logging middleware still records the
	// request line.
	handler := securityMiddleware(logMiddleware(recoverMiddleware(mux)))

	// Extension hook: let cloud/enterprise layers wrap the entire handler.
	if s.wrapRoutes != nil {
		handler = s.wrapRoutes(handler)
	}

	return handler
}

// handleFeatures returns the active feature set as JSON.
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.features)
}

// handleClawvisorKeys returns the daemon's public keys for E2E encryption.
func (s *Server) handleClawvisorKeys(w http.ResponseWriter, r *http.Request) {
	x25519Pub := base64.StdEncoding.EncodeToString(s.x25519Key.PublicKey().Bytes())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"daemon_id": s.daemonID,
		"x25519":    x25519Pub,
		"algorithm": "x25519-ecdh-aes256gcm",
	})
}

// consumeNotifierDecisions reads from the notifier's decision channel
// and routes approve/deny decisions to the appropriate handler.
func (s *Server) consumeNotifierDecisions(ctx context.Context, ch <-chan notify.CallbackDecision) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-ch:
			if !ok {
				return
			}
			var err error
			switch d.Type {
			case "approval":
				if d.Action == "approve" {
					err = s.approvalsHandler.ApproveByRequestID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.approvalsHandler.DenyByRequestID(ctx, d.TargetID, d.UserID)
				}
			case "task":
				if d.Action == "approve" {
					err = s.tasksHandler.ApproveByTaskID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.tasksHandler.DenyByTaskID(ctx, d.TargetID, d.UserID)
				}
			case "scope_expansion":
				if d.Action == "approve" {
					err = s.tasksHandler.ExpandApproveByTaskID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.tasksHandler.ExpandDenyByTaskID(ctx, d.TargetID, d.UserID)
				}
			case "connection":
				if d.Action == "approve" {
					_, err = s.connectionsHandler.ApproveByID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.connectionsHandler.DenyByID(ctx, d.TargetID, d.UserID)
				}
			}
			if err != nil {
				s.logger.Warn("notifier decision failed",
					"type", d.Type, "action", d.Action,
					"target_id", d.TargetID, "err", err)
			}
		}
	}
}

// Handler returns the HTTP handler, primarily for use in tests.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Run starts the HTTP server and blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("server running", "expired_task_filter", true)

	// Start background expiry cleanup.
	go s.approvalsHandler.RunExpiryCleanup(ctx)

	// Start SSE ticket store cleanup.
	if s.ticketStore != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.ticketStore.Cleanup()
				}
			}
		}()
	}

	// Start MCP session cleanup.
	if s.mcpServer != nil {
		s.mcpServer.StartCleanup(ctx.Done())
	}

	// Start notifier inline callback consumer and token cleanup.
	if dc, ok := s.notifier.(interface {
		DecisionChannel() <-chan notify.CallbackDecision
		RunCleanup(context.Context)
	}); ok {
		go dc.RunCleanup(ctx)

		if s.decisionBus != nil {
			// Bridge local decisions to the cross-instance bus.
			localCh := dc.DecisionChannel()
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case d, ok := <-localCh:
						if !ok {
							return
						}
						if err := s.decisionBus.Publish(ctx, d); err != nil {
							s.logger.Warn("decision bus: publish failed", "err", err)
						}
					}
				}
			}()
			// Consume decisions from the bus (receives from all instances).
			go s.consumeNotifierDecisions(ctx, s.decisionBus.Subscribe(ctx))
		} else {
			// Single-instance: consume directly from the local channel.
			go s.consumeNotifierDecisions(ctx, dc.DecisionChannel())
		}
	}

	// Start device pairing session cleanup.
	if s.devicesHandler != nil {
		go s.devicesHandler.RunCleanup(ctx)
	}

	// Start message buffer cleanup and bootstrap group observation.
	if s.msgBuffer != nil {
		go s.msgBuffer.RunCleanup(ctx)
	}

	// Start transcript cross-check anomaly detector (Stage 1 §5.4).
	// Runs every 5 minutes, looks back 15 minutes, records disagreements
	// between proxy- and plugin-sourced transcripts. Lightweight; non-
	// blocking on bridge failures; only considers proxy-enabled bridges.
	go func() {
		cc := transcript.NewCrossChecker(s.store, s.msgBuffer, s.logger)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cc.RunOnce(ctx, 15*time.Minute)
			}
		}
	}()
	if bo, ok := s.notifier.(interface {
		BootstrapGroupObservation(context.Context)
	}); ok {
		go bo.BootstrapGroupObservation(ctx)
	}

	errCh := make(chan error, 1)
	go func() {
		if !s.cfg.Server.IsLocal() {
			s.logger.Info("server starting", "addr", s.http.Addr)
		}
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("server error: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		if s.cfg.Server.IsLocal() && !s.quiet {
			fmt.Println("\n  Shutting down...")
		} else if !s.quiet {
			s.logger.Info("shutting down server")
		}
		// Close SSE connections first so handlers return before Shutdown waits on them.
		s.eventHub.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		s.store.Close()
		if !s.cfg.Server.IsLocal() {
			s.logger.Info("server stopped")
		}
		return nil
	}
}

// newKeyedLimiterFromBucket creates a KeyedLimiter from a config bucket.
// Returns nil when the bucket has zero values (unconfigured).
func newKeyedLimiterFromBucket(b config.RateLimitBucket) ratelimit.Limiter {
	if b.Limit <= 0 || b.Window <= 0 {
		return nil
	}
	return ratelimit.NewKeyedLimiter(
		rate.Limit(float64(b.Limit)/float64(b.Window)),
		b.Limit,
	)
}

// relayHostFromCfg returns the relay hostname, falling back to the default
// relay URL when config.yaml omits the relay url field.
func relayHostFromCfg(cfgURL string) string {
	u := cfgURL
	if u == "" {
		u = version.RelayURL()
	}
	return strings.TrimPrefix(strings.TrimPrefix(u, "wss://"), "ws://")
}
