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
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
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
	llmVerifier        *intent.LLMVerifier          // verdict cache cleanup target; nil when verification disabled
	cbDispatcher       *handlers.CallbackDispatcher // bounded callback delivery pool

	pushNotifier         *push.Notifier                // concrete push notifier; may be nil
	msgBuffer            groupchat.Buffer              // group chat message buffer; may be nil
	decisionBus          notify.DecisionBus            // cross-instance decision delivery; may be nil
	gatewayHooks         *GatewayHooks                 // cloud-injected gateway authorization hooks; may be nil
	feedbackHooks        *FeedbackHooks                // cloud-injected feedback event hooks; may be nil
	featuresHook         FeaturesHook                  // cloud-injected per-user feature overrides; may be nil
	localServiceProvider handlers.LocalServiceProvider // cloud-injected local daemon service provider; may be nil
	localServiceExecutor handlers.LocalServiceExecutor // cloud-injected local service executor; may be nil

	eventHub    events.EventHub
	mcpServer   *mcp.Server
	ticketStore intauth.TicketStorer

	// Multi-instance stores (set via options; nil = use defaults).
	replayCache        middleware.ReplayCache
	tokenCache         handlers.TokenCache
	claimCodeCache     handlers.ClaimCodeCache
	devicePairingStore handlers.DevicePairingStore
	oauthStateStore    handlers.OAuthStateStore
	pairingCodeStore   handlers.PairingCodeStore
	dedupCache         handlers.DedupCache
	verdictCache       intent.VerdictCacher
	extractionTracker  handlers.ExtractionTracker
	callerNonces       llmproxy.CallerNonceCache
	scriptSessions     llmproxy.ScriptSessionCache
	pendingSecrets     llmproxy.PendingSecretDecisionCache
	liteApprovals      llmproxy.PendingApprovalCache
	liteOutcomes       llmproxy.InlineApprovalOutcomeStore
	taskCheckouts      llmproxy.TaskCheckoutStore

	adapterGenFactory handlers.GeneratorFactory // per-request Generator factory; set via option

	// taskRiskAssessor scores task envelopes at creation time. Shared
	// between the dashboard task-create path (via TasksHandler) and the
	// lite-proxy inline-approval intercept so both surfaces see the
	// same LLM-judged risk read.
	taskRiskAssessor taskrisk.Assessor
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
	RuntimeProxy      bool `json:"runtime_proxy"`
	ProxyLite         bool `json:"proxy_lite"`
	SecretVault       bool `json:"secret_vault"`
	RuntimePolicyUI   bool `json:"runtime_policy_ui"`
	RuntimeActivity   bool `json:"runtime_activity"`
	AgentLiveSessions bool `json:"agent_live_sessions"`
	ServicePresets    bool `json:"service_presets"`
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

// FeaturesHook lets cloud/enterprise layers override the FeatureSet returned
// by /api/features on a per-user basis (e.g. gating features by billing plan).
// The /api/features route runs OptionalUser middleware before the hook, so
// `user` is non-nil when the request carries a valid JWT and nil otherwise
// (the pre-login bootstrap call). Hooks should return the unmodified set when
// `user` is nil.
type FeaturesHook func(ctx context.Context, user *store.User, fs FeatureSet) FeatureSet

// ServerOption configures optional behavior on the Server.
type ServerOption func(*Server)

// WithLogger uses the supplied *slog.Logger instead of constructing one from
// cfg.Server.LogFormat. Required when the caller wraps the slog handler (e.g.
// with pkg/cloudlogging) and needs that wrapping preserved on every log entry.
func WithLogger(l *slog.Logger) ServerOption {
	return func(s *Server) { s.logger = l }
}

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

// WithFeaturesHook registers a hook that may override the FeatureSet returned
// by /api/features on a per-user basis.
func WithFeaturesHook(hook FeaturesHook) ServerOption {
	return func(s *Server) { s.featuresHook = hook }
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

// WithClaimCodeCache overrides the default in-memory claim code cache.
// Multi-instance deployments must supply a shared (e.g. Redis-backed) cache
// so a code minted on one instance can be consumed on another.
func WithClaimCodeCache(cc handlers.ClaimCodeCache) ServerOption {
	return func(s *Server) { s.claimCodeCache = cc }
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

// WithExtractionTracker overrides the default in-memory extraction tracker.
// Use the Redis-backed tracker in multi-instance deployments.
func WithExtractionTracker(t handlers.ExtractionTracker) ServerOption {
	return func(s *Server) { s.extractionTracker = t }
}

// WithCallerNonceCache overrides the default in-memory caller-nonce cache
// used by the lite-proxy resolver. Use the Redis-backed cache in
// multi-instance deployments so a nonce minted on one daemon can be
// consumed on another.
func WithCallerNonceCache(c llmproxy.CallerNonceCache) ServerOption {
	return func(s *Server) { s.callerNonces = c }
}

// WithScriptSessionCache overrides the default in-memory script-session
// cache used by the autovault script-session control endpoint and the
// resolver. The default is single-process; multi-instance deployments
// should supply a shared backing cache so a session minted on one
// daemon can be authorized on another.
func WithScriptSessionCache(c llmproxy.ScriptSessionCache) ServerOption {
	return func(s *Server) { s.scriptSessions = c }
}

// WithPendingSecretDecisionCache overrides the default in-memory proxy-lite
// pending-secret cache. Use the Redis-backed cache in multi-instance
// deployments so a held secret decision can be consumed atomically anywhere.
func WithPendingSecretDecisionCache(c llmproxy.PendingSecretDecisionCache) ServerOption {
	return func(s *Server) { s.pendingSecrets = c }
}

// WithLiteApprovalCache overrides the default in-memory lite-proxy inline
// approval cache. Use a shared implementation in multi-instance deployments so
// an approve/deny reply can release a tool call held by another instance.
func WithLiteApprovalCache(c llmproxy.PendingApprovalCache) ServerOption {
	return func(s *Server) { s.liteApprovals = c }
}

// WithLiteApprovalOutcomeStore overrides the default in-memory lite-proxy
// inline approval outcome store. Use a shared implementation in multi-instance
// deployments so later turns can see approvals resolved by another instance.
func WithLiteApprovalOutcomeStore(c llmproxy.InlineApprovalOutcomeStore) ServerOption {
	return func(s *Server) { s.liteOutcomes = c }
}

// WithTaskCheckoutStore overrides the default in-memory proxy-lite task focus
// store. Use a shared implementation in multi-instance deployments so checkout
// state follows the agent across replicas.
func WithTaskCheckoutStore(c llmproxy.TaskCheckoutStore) ServerOption {
	return func(s *Server) { s.taskCheckouts = c }
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
		eventHub:   events.NewHub(),
	}

	// Apply optional configuration. WithLogger may set s.logger here, in which
	// case we skip building a default below — that preserves caller-installed
	// handler wrappers (e.g. cloudlogging).
	for _, o := range opts {
		o(s)
	}

	if s.logger == nil {
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
		s.logger = slog.New(logHandler)
	}
	slog.SetDefault(s.logger)

	if s.quiet {
		s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	mux := s.routes()

	s.http = &http.Server{
		Addr:    cfg.Server.Addr(),
		Handler: mux,
		// ReadHeaderTimeout caps how long a client may take to send the
		// request line + headers. Without it, slowloris attacks can hold
		// connections open indefinitely with one byte per second of headers.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout and IdleTimeout must exceed the long-poll cap
		// (parseLongPollTimeout) and MCP SSE idle gaps. Otherwise the
		// connection is torn down mid-handler and Cloud Run reports a 503.
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  180 * time.Second,
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
	healthHandler := handlers.NewHealthHandler(s.store, s.vault)
	configHandler := handlers.NewConfigHandler(authMode, s.cfg.ProxyLite.PublicURL)
	restrictionsHandler := handlers.NewRestrictionsHandler(s.store)
	agentsHandler := handlers.NewAgentsHandler(s.store, s.eventHub, s.logger, s.cfg)
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
		startGeminiCacheIfConfigured(s.llmCfg.Verification.LLMProviderConfig, s.logger, "verifier", v.StartGeminiCache)
		s.llmVerifier = v
		verifier = v
	}

	// Construct chain context extractor (noop if disabled).
	var extractor intent.Extractor = intent.NoopExtractor{}
	if s.llmCfg.ChainContext.Enabled {
		ext := intent.NewLLMExtractor(s.llmHealth, s.logger)
		startGeminiCacheIfConfigured(s.llmCfg.ChainContext.LLMProviderConfig, s.logger, "extractor", ext.StartGeminiCache)
		extractor = ext
	}

	// Bounded callback delivery pool — shared across all handlers that
	// dispatch agent callbacks, so a slow downstream agent can't flood
	// the daemon with goroutines.
	if s.cbDispatcher == nil {
		s.cbDispatcher = handlers.NewCallbackDispatcher(16, 1024, s.logger)
		s.cbDispatcher.Start(16)
	}

	gatewayHandler := handlers.NewGatewayHandler(
		s.store, s.vault, s.adapterReg,
		s.notifier, verifier, extractor, *s.cfg, s.logger, baseURL, s.eventHub,
	)
	gatewayHandler.SetCallbackDispatcher(s.cbDispatcher)
	if s.llmCfg.Verification.Enabled {
		gatewayHandler.SetGatewayRequestResolver(runtimepolicy.NewLLMGatewayRequestResolver(s.llmHealth, s.logger))
	}
	if s.extractionTracker != nil {
		gatewayHandler.SetExtractionTracker(s.extractionTracker)
	}
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
	vaultHandler := handlers.NewVaultHandler(s.store, s.vault, s.adapterReg)
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
	// Construct task risk assessor (noop if disabled). Stashed on the
	// server so registerLiteProxyRoutes can plumb the same instance into
	// the lite-proxy's inline-approval intercept.
	var assessor taskrisk.Assessor = taskrisk.NoopAssessor{}
	if s.llmCfg.TaskRisk.Enabled {
		a := taskrisk.NewLLMAssessor(s.llmHealth, s.adapterReg, s.logger)
		startGeminiCacheIfConfigured(s.llmCfg.TaskRisk.LLMProviderConfig, s.logger, "assessor", a.StartGeminiCache)
		assessor = a
	}
	s.taskRiskAssessor = assessor
	approvalsHandler := handlers.NewApprovalsHandler(s.store, s.vault, s.adapterReg, s.notifier, *s.cfg, assessor, s.logger, s.eventHub)
	approvalsHandler.SetCallbackDispatcher(s.cbDispatcher)
	s.approvalsHandler = approvalsHandler

	tasksHandler := handlers.NewTasksHandler(s.store, s.vault, s.adapterReg,
		s.notifier, *s.cfg, s.logger, baseURL, s.eventHub, assessor)
	tasksHandler.SetCallbackDispatcher(s.cbDispatcher)
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
	optionalUser := middleware.OptionalUser(s.jwtSvc, s.store)
	requireAgent := middleware.RequireAgent(s.store)
	logMiddleware := middleware.Logging(s.logger)
	recoverMiddleware := middleware.Recover(s.logger)
	securityMiddleware := middleware.Security(s.cfg.Server.IsLocal() && s.cfg.Server.PublicURL == "")

	// Rate limiters (skip when config is zero-valued, e.g. in tests)
	rlCfg := s.cfg.RateLimit
	gatewayRL := newKeyedLimiterFromBucket(rlCfg.Gateway)
	oauthRL := newKeyedLimiterFromBucket(rlCfg.OAuth)
	policyRL := newKeyedLimiterFromBucket(rlCfg.PolicyAPI)
	authRL := newKeyedLimiterFromBucket(rlCfg.Auth)

	// Parse trusted-proxy CIDRs once. When r.RemoteAddr falls inside any of
	// these networks, ipKeyFn honors X-Forwarded-For — otherwise the rate
	// limiter would collapse to a single bucket on hosted deployments
	// (e.g. Cloud Run) where every request appears to come from one IP.
	trustedProxyNets := make([]*net.IPNet, 0, len(s.cfg.Server.TrustedProxies))
	for _, cidr := range s.cfg.Server.TrustedProxies {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			s.logger.Warn("ignoring invalid trusted_proxies CIDR", "cidr", cidr, "err", err)
			continue
		}
		trustedProxyNets = append(trustedProxyNets, n)
	}
	ipKeyFn := func(r *http.Request) string {
		return clientIPFromRequest(r, trustedProxyNets)
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

	// Wire the gateway limiter into the handler so HandleBatch can charge
	// one token per fan-out sub-request rather than letting one batch token
	// buy N adapter calls + N LLM verifications + N audit writes.
	gatewayHandler.SetGatewayRateLimiter(gatewayRL, agentKeyFn)

	user := func(h http.HandlerFunc) http.Handler { return requireUser(h) }
	// E2E encryption middleware — wraps agent-facing routes so relay traffic is encrypted.
	// Local requests pass through unencrypted; relay requests without E2E get 403.
	e2e := func(h http.Handler) http.Handler { return h }
	if s.x25519Key != nil {
		e2eMw := middleware.E2E(s.x25519Key)
		e2e = func(h http.Handler) http.Handler { return e2eMw(h) }
	}
	userOAuthRL := func(h http.HandlerFunc) http.Handler {
		return requireUser(middleware.RateLimit(oauthRL, userKeyFn, rlCfg.OAuth.Limit)(h))
	}
	userPolicyRL := func(h http.HandlerFunc) http.Handler {
		return requireUser(middleware.RateLimit(policyRL, userKeyFn, rlCfg.PolicyAPI.Limit)(h))
	}
	llmPreAuthKeyFn := func(r *http.Request) string { return "llm-ip:" + ipKeyFn(r) }
	llmAgentKeyFn := func(r *http.Request) string {
		if a := middleware.AgentFromContext(r.Context()); a != nil {
			return "llm-agent:" + a.ID
		}
		return ""
	}
	authRateLimited := func(h http.HandlerFunc) http.Handler {
		return middleware.RateLimit(authRL, ipKeyFn, rlCfg.Auth.Limit)(h)
	}
	optionalUserAuthRateLimited := func(h http.HandlerFunc) http.Handler {
		return middleware.RateLimit(authRL, ipKeyFn, rlCfg.Auth.Limit)(optionalUser(h))
	}

	// Health (no auth)
	mux.HandleFunc("GET /health", healthHandler.Health)
	mux.HandleFunc("GET /ready", healthHandler.Ready)
	mux.HandleFunc("GET /api/config/public", configHandler.Public)
	mux.HandleFunc("GET /api/version", healthHandler.Version)
	mux.HandleFunc("GET /api/skill/version", healthHandler.SkillVersion)

	routeSet := strings.ToLower(strings.TrimSpace(s.cfg.Server.RouteSet))
	if routeSet == "proxy_lite" {
		s.registerLiteProxyRoutes(
			mux, baseURL, verifier, tasksHandler, vaultHandler, e2e, user,
			gatewayRL, llmAgentKeyFn, llmPreAuthKeyFn, rlCfg.Gateway.Limit,
			true, false,
		)
		handler := securityMiddleware(logMiddleware(recoverMiddleware(mux)))
		if s.wrapRoutes != nil {
			handler = s.wrapRoutes(handler)
		}
		return handler
	}

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
	mux.Handle("POST /api/auth/logout", optionalUserAuthRateLimited(authHandler.Logout))
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

	// Features endpoint (always registered, returns the active FeatureSet).
	// OptionalUser populates the request user if a valid token is present so
	// the FeaturesHook can return a per-user FeatureSet; pre-login callers see
	// the deployment-level set.
	mux.Handle("GET /api/features", middleware.OptionalUser(s.jwtSvc, s.store)(http.HandlerFunc(s.handleFeatures)))

	// Restrictions (rate-limited writes)
	mux.Handle("GET /api/restrictions", user(restrictionsHandler.List))
	mux.Handle("POST /api/restrictions", userPolicyRL(restrictionsHandler.Create))
	mux.Handle("DELETE /api/restrictions/{id}", userPolicyRL(restrictionsHandler.Delete))

	// Agents (user JWT)
	mux.Handle("GET /api/agents", user(agentsHandler.List))
	mux.Handle("POST /api/agents", user(agentsHandler.Create))
	mux.Handle("POST /api/agents/{id}/rotate", user(agentsHandler.RotateToken))
	mux.Handle("GET /api/agents/{id}/runtime-settings", user(agentsHandler.GetRuntimeSettings))
	mux.Handle("PUT /api/agents/{id}/runtime-settings", user(agentsHandler.UpdateRuntimeSettings))
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

	// Connection requests (unauthenticated — agents requesting access)
	connectionsHandler := handlers.NewConnectionsHandler(s.store, s.notifier, s.eventHub, s.logger, baseURL, s.features.MultiTenant)
	if s.tokenCache != nil {
		connectionsHandler.SetTokenCache(s.tokenCache)
	}
	if s.claimCodeCache != nil {
		connectionsHandler.SetClaimCodeCache(s.claimCodeCache)
	}
	s.connectionsHandler = connectionsHandler
	connectionsRL := newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 10, Window: 60})
	mux.Handle("POST /api/agents/connect",
		middleware.RateLimit(connectionsRL, ipKeyFn, 10)(e2e(http.HandlerFunc(connectionsHandler.RequestConnect))))
	mux.Handle("GET /api/agents/connect/{id}/status", e2e(http.HandlerFunc(connectionsHandler.PollStatus)))

	// Connection request management (user JWT)
	// Claim-code minting supports proxy-lite bootstrap curls; keep the
	// route absent for proxy-lite-disabled installs so the connect API
	// surface matches main.
	if s.cfg.ProxyLite.Enabled {
		claimMintRL := newKeyedLimiterFromBucket(config.RateLimitBucket{Limit: 30, Window: 60})
		mux.Handle("POST /api/agents/connect/claim",
			requireUser(middleware.RateLimit(claimMintRL, userKeyFn, 30)(http.HandlerFunc(connectionsHandler.MintClaim))))
	}
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
	// Batch endpoint: N sub-requests in one round-trip. The route-level
	// middleware consumes one token for the batch envelope; HandleBatch
	// then charges one additional token per fan-out sub-request via the
	// limiter wired in by SetGatewayRateLimiter, so a 20-request batch
	// consumes 20 tokens — not 1. See TestBatch_RateLimitChargesPerSubRequest.
	mux.Handle("POST /api/gateway/batch", requireAgent(middleware.RateLimit(gatewayRL, agentKeyFn, rlCfg.Gateway.Limit)(
		e2e(http.HandlerFunc(gatewayHandler.HandleBatch)))))

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

	// Services / OAuth (user JWT, rate-limited)
	mux.Handle("GET /api/services", user(servicesHandler.List))
	if s.cfg.ProxyLite.Enabled {
		mux.Handle("GET /api/vault/items", user(vaultHandler.ListForUser))
		mux.Handle("POST /api/vault/items", user(vaultHandler.CreateForUser))
		mux.Handle("GET /api/vault/items/{id}", user(vaultHandler.GetForUser))
		mux.Handle("PUT /api/vault/items/{id}", user(vaultHandler.UpdateForUser))
		mux.Handle("DELETE /api/vault/items/{id}", user(vaultHandler.DeleteForUser))
		mux.Handle("GET /api/agent/vault/items", requireAgent(e2e(http.HandlerFunc(vaultHandler.ListForAgent))))
	}
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
	mux.Handle("GET /api/system/microsoft-oauth", user(servicesHandler.GetMicrosoftOAuthConfig))
	mux.Handle("POST /api/system/microsoft-oauth", user(servicesHandler.SetMicrosoftOAuthConfig))
	mux.Handle("GET /api/system/pkce-credentials", user(servicesHandler.ListPKCECredentials))
	mux.Handle("POST /api/system/pkce-credentials", user(servicesHandler.SetPKCECredential))
	mux.Handle("DELETE /api/system/pkce-credentials/{service_id}", user(servicesHandler.DeletePKCECredential))
	mux.Handle("GET /api/system/mcp-oauth", user(servicesHandler.ListMCPOAuthCredentials))
	mux.Handle("POST /api/system/mcp-oauth", user(servicesHandler.SetMCPOAuthCredential))
	mux.Handle("DELETE /api/system/mcp-oauth/{service_id}", user(servicesHandler.DeleteMCPOAuthCredential))

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
	mux.Handle("POST /api/tasks/{id}/start", requireAgent(e2e(http.HandlerFunc(tasksHandler.Start))))
	mux.Handle("POST /api/tasks/{id}/end", requireAgent(e2e(http.HandlerFunc(tasksHandler.End))))
	mux.Handle("POST /api/tasks/{id}/complete", requireAgent(e2e(http.HandlerFunc(tasksHandler.Complete))))
	mux.Handle("POST /api/tasks/{id}/expand", requireAgent(e2e(http.HandlerFunc(tasksHandler.Expand))))

	// Tasks (user JWT)
	mux.Handle("GET /api/tasks", user(tasksHandler.List))
	mux.Handle("GET /api/tasks/{id}/cost", user(tasksHandler.Cost))
	mux.Handle("POST /api/tasks/{id}/approve", user(tasksHandler.Approve))
	mux.Handle("PATCH /api/tasks/{id}/scope", user(tasksHandler.UpdateScope))
	mux.Handle("POST /api/tasks/{id}/deny", user(tasksHandler.Deny))
	mux.Handle("POST /api/tasks/{id}/revoke", user(tasksHandler.Revoke))
	mux.Handle("POST /api/tasks/{id}/expand/approve", user(tasksHandler.ExpandApprove))
	mux.Handle("POST /api/tasks/{id}/expand/deny", user(tasksHandler.ExpandDeny))

	// Audit (user JWT)
	mux.Handle("GET /api/audit", user(auditHandler.List))
	mux.Handle("GET /api/audit/{id}", user(auditHandler.Get))
	mux.Handle("GET /api/audit/mutes", user(auditHandler.ListMutes))
	mux.Handle("POST /api/audit/mutes", user(auditHandler.CreateMute))
	mux.Handle("DELETE /api/audit/mutes/{id}", user(auditHandler.DeleteMute))

	if s.cfg.ProxyLite.Enabled {
		s.registerLiteProxyRoutes(
			mux, baseURL, verifier, tasksHandler, vaultHandler, e2e, user,
			gatewayRL, llmAgentKeyFn, llmPreAuthKeyFn, rlCfg.Gateway.Limit,
			routeSet != "app", true,
		)
	}
	mux.HandleFunc("/v1/", http.NotFound)
	mux.HandleFunc("/proxy/v1/", http.NotFound)
	mux.HandleFunc("/control", http.NotFound)
	mux.HandleFunc("/control/", http.NotFound)

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
		mux.HandleFunc("GET /skill/setup", onboardingHandler.Setup)
		mux.HandleFunc("GET /skill/clawvisor-setup.md", onboardingHandler.ClaudeCodeSetup)

		// Per-harness installer skills — one markdown doc per target (claude-code,
		// codex, hermes, openclaw). Each renders with a pre-filled CLAWVISOR_URL
		// and optional ?claim=<code> so the embedded mint curl doesn't need a
		// user_id. The Other Agents fallback path still uses /skill/setup.
		installerHandler := handlers.NewInstallerHandler(relayHost, s.daemonID, s.cfg.Server.IsLocal(), s.cfg.ProxyLite.PublicURL, s.cfg.Server.PublicURL)
		mux.HandleFunc("GET /skill/install/{target}", installerHandler.Setup)
		// Companion uninstall route — the install skill writes the rendered
		// markdown to ~/.claude/commands/clawvisor-uninstall.md so users have a
		// one-command revert path (/clawvisor-uninstall).
		mux.HandleFunc("GET /skill/uninstall/{target}", installerHandler.Uninstall)
	}

	// Claude Desktop configuration profile (.mobileconfig) — the user
	// downloads the file, double-clicks it, macOS installs the managed
	// config and Claude Desktop reads it. The endpoint mints a fresh agent
	// + token at request time; the download itself is the consent gate, so
	// it requires the user JWT.
	mobileConfigHandler := handlers.NewMobileConfigHandler(s.store, relayHostFromCfg(s.cfg.Relay.URL), s.daemonID, s.cfg.Server.IsLocal(), s.cfg.ProxyLite.PublicURL)
	mux.Handle("GET /api/agents/install/claude-desktop.mobileconfig", user(mobileConfigHandler.ClaudeDesktop))
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
			"POST /api/tasks/{id}/start":                     http.HandlerFunc(tasksHandler.Start),
			"POST /api/tasks/{id}/end":                       http.HandlerFunc(tasksHandler.End),
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
			// codeql[go/path-injection] This only chooses SPA fallback; actual static serving is delegated to http.FileServer rooted at FrontendDir.
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

func (s *Server) registerLiteProxyRoutes(
	mux *http.ServeMux,
	baseURL string,
	verifier intent.Verifier,
	tasksHandler *handlers.TasksHandler,
	vaultHandler *handlers.VaultHandler,
	e2e func(http.Handler) http.Handler,
	user func(http.HandlerFunc) http.Handler,
	gatewayRL ratelimit.Limiter,
	llmAgentKeyFn func(*http.Request) string,
	llmPreAuthKeyFn func(*http.Request) string,
	gatewayLimit int,
	includeProxySurface bool,
	includeCredentialRoutes bool,
) {
	var callerNonces llmproxy.CallerNonceCache
	if includeProxySurface {
		llmHandler := handlers.NewLLMEndpointHandler(s.store, s.vault, s.logger)
		if v := s.cfg.ProxyLite.AnthropicBaseURL; v != "" {
			llmHandler.Forwarder.Upstream.AnthropicBaseURL = v
		}
		if v := s.cfg.ProxyLite.OpenAIBaseURL; v != "" {
			llmHandler.Forwarder.Upstream.OpenAIBaseURL = v
		}

		var validator inspector.Validator = inspector.AmbiguousValidator{}
		if s.cfg.LLM.Verification.Enabled {
			validator = inspector.NewLLMClientValidator(s.llmHealth.VerificationConfig, s.logger)
			llmHandler.SecretAdjudicator = runtimeautovault.NewLLMSecretAdjudicator(s.llmHealth.VerificationConfig, s.logger)
		}
		llmHandler.Inspector = inspector.NewInspector(inspector.DefaultParser{}, validator)

		tracePath := s.cfg.ProxyLite.TraceLogPath
		if env := strings.TrimSpace(os.Getenv("CLAWVISOR_PROXY_LITE_TRACE")); env != "" {
			tracePath = env
		}
		if traceLogger, err := llmproxy.OpenTraceLogger(tracePath); err != nil {
			s.logger.Warn("lite-proxy: failed to open trace log", "path", tracePath, "err", err.Error())
		} else if traceLogger != nil {
			llmHandler.TraceLogger = traceLogger
			s.logger.Info("lite-proxy: decision trace enabled", "path", tracePath)
		}

		rawLogPath := s.cfg.ProxyLite.RawLogPath
		if env := strings.TrimSpace(os.Getenv("CLAWVISOR_PROXY_LITE_RAW_LOG")); env != "" {
			rawLogPath = env
		}
		if rawLogger, err := llmproxy.OpenRawIOLogger(rawLogPath); err != nil {
			s.logger.Warn("lite-proxy: failed to open raw-io log", "path", rawLogPath, "err", err.Error())
		} else if rawLogger != nil {
			llmHandler.RawIOLogger = rawLogger
			s.logger.Info("lite-proxy: raw I/O log enabled", "path", rawLogPath)
		}

		resolverBase := s.cfg.Server.PublicURL
		if resolverBase == "" {
			resolverBase = baseURL
		}
		llmHandler.ResolverBaseURL = strings.TrimRight(resolverBase, "/") + "/api/proxy"
		llmHandler.ControlBaseURL = strings.TrimRight(baseURL, "/")
		// Dashboard host for the "vault a key" deep link surfaced on
		// upstream-credential errors. In split-mode hosted deploys
		// (route_set: proxy_lite) baseURL points at the proxy itself
		// (e.g. llm.clawvisor.com), so fall back to the build-env
		// dashboard URL. Everywhere else baseURL IS the dashboard.
		if strings.EqualFold(strings.TrimSpace(s.cfg.Server.RouteSet), "proxy_lite") {
			llmHandler.DashboardBaseURL = version.DashboardURL()
		} else {
			llmHandler.DashboardBaseURL = strings.TrimRight(baseURL, "/")
		}

		auditEmitter := llmproxy.NewAuditEmitter(s.store, s.logger, nil)
		llmHandler.AuditEmitter = auditEmitter
		llmHandler.Catalog = llmproxy.NewLazyServiceCatalog(llmproxy.DefsFromRegistry(s.adapterReg))
		llmHandler.TaskScope = llmproxy.NewStoreTaskScopeChecker(s.store)
		llmHandler.IntentVerifier = llmproxy.NewCircuitBreakerVerifier(
			llmproxy.NewIntentVerifierAdapter(verifier),
			llmproxy.DefaultCircuitBreakerConfig(),
		)

		resolverHandler := handlers.NewProxyResolverHandler(s.store, s.vault, s.logger)
		resolverHandler.AdapterReg = s.adapterReg
		resolverHandler.SelfHostnames = s.cfg.ProxyLite.SelfHostnames
		resolverHandler.AllowPrivateNetworks = s.cfg.ProxyLite.AllowPrivateNetworks
		resolverHandler.AuditEmitter = auditEmitter

		callerNonces = s.callerNonces
		if callerNonces == nil {
			callerNonces = llmproxy.NewMemoryCallerNonceCache(5 * time.Minute)
			if s.logger != nil && strings.EqualFold(strings.TrimSpace(s.cfg.Server.RouteSet), "proxy_lite") {
				s.logger.Warn("lite-proxy: CallerNonceCache not configured — resolver nonces are process-local; use Redis for multi-instance proxy deployments")
			}
		}
		llmHandler.CallerNonces = callerNonces

		scriptSessions := s.scriptSessions
		if scriptSessions == nil {
			scriptSessions = llmproxy.NewMemoryScriptSessionCache()
			if s.logger != nil && strings.EqualFold(strings.TrimSpace(s.cfg.Server.RouteSet), "proxy_lite") {
				s.logger.Warn("lite-proxy: ScriptSessionCache not configured — autovault script sessions are process-local; use a shared backing cache for multi-instance proxy deployments")
			}
		}
		// resolver no longer holds a ScriptSessionCache field — it
		// receives the cache via the request context (attached by the
		// nonce middleware below), so the cache used to release the
		// reservation is structurally the same one that took it.
		if s.pendingSecrets != nil {
			llmHandler.PendingSecrets = s.pendingSecrets
		}

		if s.liteApprovals != nil {
			llmHandler.PendingApprovals = s.liteApprovals
		} else if s.logger != nil && strings.EqualFold(strings.TrimSpace(s.cfg.Server.RouteSet), "proxy_lite") {
			s.logger.Warn("lite-proxy: LiteApprovalCache not configured — inline approvals are process-local; use Redis for multi-instance proxy deployments")
		}
		if s.liteOutcomes != nil {
			llmHandler.InlineApprovalOutcomes = s.liteOutcomes
		} else if s.logger != nil && strings.EqualFold(strings.TrimSpace(s.cfg.Server.RouteSet), "proxy_lite") {
			s.logger.Warn("lite-proxy: LiteApprovalOutcomeStore not configured — inline approval outcomes are process-local; use Redis for multi-instance proxy deployments")
		}
		if s.taskCheckouts != nil {
			llmHandler.TaskCheckouts = s.taskCheckouts
		} else if s.logger != nil && strings.EqualFold(strings.TrimSpace(s.cfg.Server.RouteSet), "proxy_lite") {
			s.logger.Warn("lite-proxy: TaskCheckoutStore not configured — task focus is process-local; use Redis for multi-instance proxy deployments")
		}
		llmHandler.InlineTaskCreator = tasksHandler
		llmHandler.TaskRiskAssessor = s.taskRiskAssessor
		llmHandler.DefaultTaskExpirySeconds = s.cfg.Task.DefaultExpirySeconds

		controlHandler := handlers.NewLLMControlHandler(baseURL)
		controlHandler.Store = s.store
		controlHandler.TaskCheckouts = llmHandler.TaskCheckouts
		controlHandler.Audit = auditEmitter
		controlHandler.ScriptSessions = scriptSessions
		controlHandler.IntentVerifier = verifier
		requireAgentLLM := middleware.RequireAgentLLM(s.store)
		requireAgentLLMRL := func(h http.HandlerFunc) http.Handler {
			agentLimited := middleware.RateLimit(gatewayRL, llmAgentKeyFn, gatewayLimit)(http.HandlerFunc(h))
			authenticated := requireAgentLLM(agentLimited)
			return middleware.RateLimit(gatewayRL, llmPreAuthKeyFn, gatewayLimit)(authenticated)
		}
		nonceMW := middleware.RequireAgentLLMNonce(s.store, callerNonces, scriptSessions, s.logger)
		requireAgentLLMCaller := func(h http.Handler) http.Handler {
			return middleware.RateLimit(gatewayRL, llmPreAuthKeyFn, gatewayLimit)(nonceMW(h))
		}

		mux.Handle("POST /api/v1/messages", requireAgentLLMRL(llmHandler.Messages))
		mux.Handle("POST /api/v1/messages/count_tokens", requireAgentLLMRL(llmHandler.Messages))
		mux.Handle("POST /api/v1/chat/completions", requireAgentLLMRL(llmHandler.ChatCompletions))
		mux.Handle("POST /api/v1/responses", requireAgentLLMRL(llmHandler.Responses))

		mux.Handle("GET /api/control", http.HandlerFunc(controlHandler.Capabilities))
		mux.Handle("GET /api/control/capabilities", http.HandlerFunc(controlHandler.Capabilities))
		mux.Handle("GET /api/control/skill", http.HandlerFunc(controlHandler.Skill))
		mux.Handle("POST /api/control/failure", requireAgentLLMCaller(e2e(http.HandlerFunc(controlHandler.Failure))))
		mux.Handle("GET /api/control/tasks", requireAgentLLMCaller(e2e(http.HandlerFunc(controlHandler.ListTasks))))
		mux.Handle("POST /api/control/tasks", requireAgentLLMCaller(e2e(http.HandlerFunc(tasksHandler.Create))))
		mux.Handle("POST /api/control/task/checkout", requireAgentLLMCaller(e2e(http.HandlerFunc(controlHandler.CheckoutTask))))
		mux.Handle("GET /api/control/tasks/{id}", requireAgentLLMCaller(e2e(http.HandlerFunc(tasksHandler.Get))))
		mux.Handle("POST /api/control/tasks/{id}/expand", requireAgentLLMCaller(e2e(http.HandlerFunc(tasksHandler.Expand))))
		mux.Handle("GET /api/control/vault/items", requireAgentLLMCaller(e2e(http.HandlerFunc(vaultHandler.ListForAgent))))
		mux.Handle("GET /api/control/vault/items/{id}", requireAgentLLMCaller(e2e(http.HandlerFunc(vaultHandler.GetForAgent))))
		// AutovaultScriptDocs is intentionally unauthenticated, matching
		// the other static documentation surfaces on the control plane
		// (/api/control, /api/control/capabilities, /api/control/skill).
		// The payload is purely static metadata about the script-session
		// protocol — endpoint shapes, hard-limit constants, example
		// headers. It reveals the deployment's resolver base URL
		// (derived from h.BaseURL) and the cap constants, both of which
		// are also discoverable from the LLM-control-notice text any
		// authenticated agent already sees. Caller-auth on this route
		// would just add latency without adding confidentiality.
		mux.Handle("GET /api/control/autovault/script", http.HandlerFunc(controlHandler.AutovaultScriptDocs))
		mux.Handle("POST /api/control/autovault/script-session", requireAgentLLMCaller(e2e(http.HandlerFunc(controlHandler.MintScriptSession))))
		mux.Handle("/api/control/", requireAgentLLMCaller(e2e(http.HandlerFunc(controlHandler.NotFound))))

		mux.Handle("/api/proxy/", requireAgentLLMCaller(http.HandlerFunc(resolverHandler.Forward)))
	}

	if includeCredentialRoutes {
		llmCredHandler := handlers.NewLLMCredentialsHandler(s.store, s.vault, s.logger)
		// Accept either a user JWT or a `cvis_…` agent token. The one-paste
		// install skill (which holds the freshly-minted agent token but no
		// dashboard session) vaults the user's upstream LLM key from inside
		// the skill via this endpoint; the dashboard's Settings UI still
		// uses the same routes with user-JWT auth.
		requireUserOrAgent := middleware.RequireUserOrAgent(s.jwtSvc, s.store)
		userOrAgent := func(h http.HandlerFunc) http.Handler { return requireUserOrAgent(h) }
		mux.Handle("PUT /api/runtime/llm-credentials/{provider}", userOrAgent(llmCredHandler.Set))
		mux.Handle("DELETE /api/runtime/llm-credentials/{provider}", userOrAgent(llmCredHandler.Delete))
		mux.Handle("GET /api/runtime/llm-credentials", userOrAgent(llmCredHandler.List))
	}
}

// handleFeatures returns the active feature set as JSON.
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	fs := s.features
	if s.featuresHook != nil {
		fs = s.featuresHook(r.Context(), middleware.UserFromContext(r.Context()), fs)
	}
	w.Header().Set("Content-Type", "application/json")
	// The response varies per-user (via featuresHook). Without no-store, a
	// browser may cache the anonymous response and serve it back after the
	// user logs in, since Authorization is not part of the default cache key.
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(fs)
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
				// d.TaskID disambiguates sibling pending approvals that share
				// request_id under symmetric dedup. Telegram callback tokens
				// carry it through; older clients leave it empty, in which
				// case the handlers fall back to the request_id-only lookup
				// and surface ErrAmbiguous if more than one row matches.
				if d.Action == "approve" {
					err = s.approvalsHandler.ApproveByRequestID(ctx, d.TargetID, d.UserID, d.TaskID)
				} else {
					err = s.approvalsHandler.DenyByRequestID(ctx, d.TargetID, d.UserID, d.TaskID)
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
				if s.connectionsHandler == nil {
					err = fmt.Errorf("connection decisions are unavailable in route_set=%q", s.cfg.Server.RouteSet)
				} else if d.Action == "approve" {
					_, err = s.connectionsHandler.ApproveByID(ctx, d.TargetID, d.UserID)
				} else {
					err = s.connectionsHandler.DenyByID(ctx, d.TargetID, d.UserID)
				}
			}
			if err != nil {
				s.logger.WarnContext(ctx, "notifier decision failed",
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

	// Start verdict-cache cleanup so the in-memory cache evicts expired
	// entries on a schedule rather than only on-access.
	if s.llmVerifier != nil {
		go s.llmVerifier.RunCleanup(ctx)
	}

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
		// Drain in-flight callback deliveries before closing the store so
		// the dispatcher's worker context still has a usable backend if it
		// races into a final delivery attempt.
		if s.cbDispatcher != nil {
			s.cbDispatcher.Stop()
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

// startGeminiCacheIfConfigured runs the Gemini explicit-context-cache start
// dance for an LLM-backed component (verifier, extractor, etc.). No-op
// unless the provider is "gemini" and a cache is configured. Failures are
// logged and swallowed — the caller continues uncached. The cache resource
// auto-expires by TTL on Vertex's side, so no shutdown hook is needed.
func startGeminiCacheIfConfigured(
	cfg config.LLMProviderConfig,
	logger *slog.Logger,
	label string,
	start func(context.Context, llm.GeminiCacheManagerConfig) error,
) {
	if cfg.Provider != "gemini" || cfg.GeminiCache == nil || !cfg.GeminiCache.Enabled {
		return
	}
	cacheRegion := cfg.GeminiCache.Region
	if cacheRegion == "" {
		cacheRegion = cfg.Region
	}
	if cacheRegion != "" && cfg.Region != "" && cacheRegion != cfg.Region {
		// Cross-region caching works for some Vertex models (notably
		// global-only preview models with the cache pinned to a real
		// region) but isn't broadly documented. Log a warning so the
		// configuration is visible if requests start failing.
		logger.Warn("gemini "+label+" cache region differs from inference region",
			"cache_region", cacheRegion, "inference_region", cfg.Region)
	}
	cacheCfg := llm.GeminiCacheManagerConfig{
		Project: cfg.Project,
		Region:  cacheRegion,
		Model:   cfg.Model,
		TTL:     time.Duration(cfg.GeminiCache.TTLSeconds) * time.Second,
		Logger:  logger,
	}
	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := start(startCtx, cacheCfg); err != nil {
		logger.Error("gemini "+label+" cache start failed; running uncached",
			"err", err)
	}
}
