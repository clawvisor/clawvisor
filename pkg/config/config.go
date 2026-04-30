package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/pkg/version"
)

// AutoConfigured tracks which settings were auto-resolved (not explicitly set).
type AutoConfigured struct {
	DatabaseDriver bool
	JWTSecret      bool
}

type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Database      DatabaseConfig      `yaml:"database"`
	Vault         VaultConfig         `yaml:"vault"`
	Auth          AuthConfig          `yaml:"auth"`
	Approval      ApprovalConfig      `yaml:"approval"`
	Callback      CallbackConfig      `yaml:"callback"`
	Task          TaskConfig          `yaml:"task"`
	Gateway       GatewayConfig       `yaml:"gateway"`
	LLM           LLMConfig           `yaml:"llm"`
	MCP           MCPConfig           `yaml:"mcp"`
	RuntimeProxy  RuntimeProxyConfig  `yaml:"runtime_proxy"`
	RuntimePolicy RuntimePolicyConfig `yaml:"runtime_policy"`
	RateLimit     RateLimitConfig     `yaml:"rate_limit"`
	Relay         RelayConfig         `yaml:"relay"`
	Telemetry     TelemetryConfig     `yaml:"telemetry"`
	Daemon        DaemonConfig        `yaml:"daemon"`
	Push          PushConfig          `yaml:"push"`
	AutoUpdate    AutoUpdateConfig    `yaml:"auto_update"`
	Redis         RedisConfig         `yaml:"redis"`

	AutoConfig AutoConfigured `yaml:"-"`
}

// GatewayConfig holds settings for the gateway request handler.
type GatewayConfig struct {
	ContentDedupTTLSeconds int `yaml:"content_dedup_ttl_seconds"` // default: 5
	NPSSamplePercent       int `yaml:"nps_sample_percent"`        // 0-100, default: 1
}

// RelayConfig holds settings for the cloud relay connection.
type RelayConfig struct {
	URL                string `yaml:"url"`                  // wss://relay.clawvisor.com
	DaemonID           string `yaml:"daemon_id"`            // assigned on registration
	KeyFile            string `yaml:"key_file"`             // Ed25519 private key path (relay auth)
	E2EKeyFile         string `yaml:"e2e_key_file"`         // X25519 private key path (E2E encryption)
	ReconnectBaseDelay string `yaml:"reconnect_base_delay"` // default: 1s
	ReconnectMaxDelay  string `yaml:"reconnect_max_delay"`  // default: 60s
	Enabled            bool   `yaml:"enabled"`              // default: false (explicit opt-in)
}

// DaemonConfig holds settings for daemon mode.
type DaemonConfig struct {
	DataDir string `yaml:"data_dir"` // defaults to ~/.clawvisor
	LogFile string `yaml:"log_file"` // relative to data_dir, defaults to logs/daemon.log
}

// PushConfig holds settings for mobile push notifications.
type PushConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
}

// AutoUpdateConfig holds settings for automatic binary updates.
// Disabled by default — self-hosted users must opt in.
type AutoUpdateConfig struct {
	Enabled       bool   `yaml:"enabled"`        // default: false (explicit opt-in)
	CheckInterval string `yaml:"check_interval"` // default: "6h"
}

// CheckIntervalDuration parses the configured check interval.
func (a AutoUpdateConfig) CheckIntervalDuration() (time.Duration, error) {
	if a.CheckInterval == "" {
		return 6 * time.Hour, nil
	}
	return time.ParseDuration(a.CheckInterval)
}

// RedisConfig holds settings for Redis (used in cloud/multi-instance deployments).
type RedisConfig struct {
	URL string `yaml:"url"` // e.g. "redis://localhost:6379"
}

// TelemetryConfig holds settings for anonymous usage telemetry.
type TelemetryConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

// CallbackConfig holds settings for callback delivery.
type CallbackConfig struct {
	AllowPrivateCIDRs []string `yaml:"allow_private_cidrs"` // CIDRs exempt from SSRF callback blocking
	RequireHTTPS      bool     `yaml:"require_https"`       // when true, reject http:// callbacks except for localhost
}

// TaskConfig holds settings for task-scoped authorization.
type TaskConfig struct {
	DefaultExpirySeconds int `yaml:"default_expiry_seconds"` // default: 1800 (30 min)
}

type ServerConfig struct {
	Port        int    `yaml:"port"`
	Host        string `yaml:"host"`
	FrontendDir string `yaml:"frontend_dir"`
	PublicURL   string `yaml:"public_url"` // e.g. "http://192.168.4.247:5173" — used in Telegram notification links
	AuthMode    string `yaml:"auth_mode"`  // "magic_link", "password", or "" (auto-detect from IsLocal)
	LogFormat   string `yaml:"log_format"` // "json", "text", or "" (auto: json in prod, text in dev)
	LogLevel    string `yaml:"log_level"`  // "debug", "info", "warn", "error" (default: "info")
}

type DatabaseConfig struct {
	Driver      string `yaml:"driver"`
	PostgresURL string `yaml:"postgres_url"`
	SQLitePath  string `yaml:"sqlite_path"`
}

type VaultConfig struct {
	Backend      string `yaml:"backend"`
	LocalKeyFile string `yaml:"local_key_file"`
	GCPProject   string `yaml:"gcp_project"`
	MasterKey    string `yaml:"-"` // base64-encoded 32-byte key; env-only (VAULT_KEY)
}

type AuthConfig struct {
	JWTSecret       string   `yaml:"jwt_secret"`
	AccessTokenTTL  string   `yaml:"access_token_ttl"`
	RefreshTokenTTL string   `yaml:"refresh_token_ttl"`
	AllowedEmails   []string `yaml:"allowed_emails"`
	MaxUsers        int      `yaml:"max_users"` // 0 = unlimited
}

type ApprovalConfig struct {
	Timeout   int    `yaml:"timeout"`
	OnTimeout string `yaml:"on_timeout"` // Reserved for Phase 9: behavior when approval times out ("fail" or "allow")
}

// LLMProviderConfig holds settings for one LLM provider endpoint.
type LLMProviderConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Provider       string `yaml:"provider"` // "openai" (default) | "anthropic"
	Endpoint       string `yaml:"endpoint"` // Base URL, e.g. https://api.anthropic.com/v1
	APIKey         string `yaml:"api_key"`  // Overridable via env
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	SkipReadonly   bool   `yaml:"skip_readonly"` // Safety only: skip check for read actions
}

// VerificationConfig holds settings for intent verification.
type VerificationConfig struct {
	LLMProviderConfig `yaml:",inline"`
	FailClosed        bool `yaml:"fail_closed"`
	CacheTTLSeconds   int  `yaml:"cache_ttl_seconds"`
}

// TaskRiskConfig holds settings for task risk assessment.
type TaskRiskConfig struct {
	LLMProviderConfig `yaml:",inline"`
}

// ChainContextConfig holds settings for chain context extraction.
type ChainContextConfig struct {
	LLMProviderConfig `yaml:",inline"`
}

// AdapterGenConfig holds settings for LLM-powered adapter generation.
type AdapterGenConfig struct {
	LLMProviderConfig `yaml:",inline"`
}

// FeedbackReviewConfig holds settings for LLM-powered agent feedback review.
type FeedbackReviewConfig struct {
	LLMProviderConfig `yaml:",inline"`
}

// LLMConfig groups all LLM provider configurations.
// Shared fields (provider, endpoint, api_key, model, timeout_seconds) are inherited
// by subsections unless explicitly overridden at the subsection level.
type LLMConfig struct {
	Provider       string `yaml:"provider"`        // Shared default: "anthropic"
	Endpoint       string `yaml:"endpoint"`        // Shared default: "https://api.anthropic.com/v1"
	APIKey         string `yaml:"api_key"`         // Shared default; overridable via CLAWVISOR_LLM_API_KEY
	Model          string `yaml:"model"`           // Shared default: "claude-haiku-4-5-20251001"
	TimeoutSeconds int    `yaml:"timeout_seconds"` // Shared default: 10

	Verification   VerificationConfig   `yaml:"verification"`    // Intent verification (runtime)
	TaskRisk       TaskRiskConfig       `yaml:"task_risk"`       // Task risk assessment (creation time)
	ChainContext   ChainContextConfig   `yaml:"chain_context"`   // Chain context extraction (multi-step tasks)
	AdapterGen     AdapterGenConfig     `yaml:"adapter_gen"`     // LLM-powered adapter generation
	FeedbackReview FeedbackReviewConfig `yaml:"feedback_review"` // Agent feedback report review
}

// MCPConfig holds settings for the MCP server.
type MCPConfig struct {
	Enabled         bool `yaml:"enabled"`          // default: true
	ApprovalTimeout int  `yaml:"approval_timeout"` // MCP tool call approval timeout in seconds (default 240s)
	SessionTTL      int  `yaml:"session_ttl"`      // session TTL in minutes (default: 1440 = 24h)
}

type RuntimeProxyConfig struct {
	Enabled            bool     `yaml:"enabled"`
	ListenAddr         string   `yaml:"listen_addr"`
	DataDir            string   `yaml:"data_dir"`
	TLS                bool     `yaml:"tls"`
	ListenerHostnames  []string `yaml:"listener_hostnames"`
	SessionTTLSeconds  int      `yaml:"session_ttl_seconds"`
	TimingTraceEnabled bool     `yaml:"timing_trace_enabled"`
	TimingTraceDir     string   `yaml:"timing_trace_dir"`
}

type RuntimePolicyConfig struct {
	ObservationModeDefault  bool     `yaml:"observation_mode_default"`
	InlineApprovalEnabled   bool     `yaml:"inline_approval_enabled"`
	HarnessAllowlist        []string `yaml:"harness_allowlist"`
	ToolLeaseTimeoutSeconds int      `yaml:"tool_lease_timeout_seconds"`
	OneOffTTLSeconds        int      `yaml:"one_off_ttl_seconds"`
	AutovaultMode           string   `yaml:"autovault_mode"`
	InjectStoredBearer      bool     `yaml:"inject_stored_bearer"`
}

// RateLimitBucket configures a single rate limit bucket.
type RateLimitBucket struct {
	Limit  int `yaml:"limit"`  // max requests per window
	Window int `yaml:"window"` // window in seconds
}

// RateLimitConfig holds rate limit settings for different route groups.
type RateLimitConfig struct {
	Gateway   RateLimitBucket `yaml:"gateway"`    // per agent
	OAuth     RateLimitBucket `yaml:"oauth"`      // per user
	PolicyAPI RateLimitBucket `yaml:"policy_api"` // per user
	ReviewRun RateLimitBucket `yaml:"review_run"` // per user
	Auth      RateLimitBucket `yaml:"auth"`       // per IP (pre-auth endpoints)
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port: 25297,
			Host: "127.0.0.1",
		},
		Database: DatabaseConfig{
			Driver:     "",
			SQLitePath: "./clawvisor.db",
		},
		Vault: VaultConfig{
			Backend:      "local",
			LocalKeyFile: "./vault.key",
		},
		Auth: AuthConfig{
			AccessTokenTTL:  "15m",
			RefreshTokenTTL: "720h",
		},
		Approval: ApprovalConfig{
			Timeout:   300,
			OnTimeout: "fail",
		},
		Task: TaskConfig{
			DefaultExpirySeconds: 1800,
		},
		Gateway: GatewayConfig{
			ContentDedupTTLSeconds: 5,
			NPSSamplePercent:       1,
		},
		LLM: LLMConfig{
			Provider:       "anthropic",
			Endpoint:       "https://api.anthropic.com/v1",
			Model:          "claude-haiku-4-5-20251001",
			TimeoutSeconds: 10,
			Verification: VerificationConfig{
				LLMProviderConfig: LLMProviderConfig{
					TimeoutSeconds: 15,
				},
				FailClosed:      true,
				CacheTTLSeconds: 60,
			},
			TaskRisk: TaskRiskConfig{
				LLMProviderConfig: LLMProviderConfig{
					Enabled: true,
				},
			},
			AdapterGen: AdapterGenConfig{
				LLMProviderConfig: LLMProviderConfig{
					Enabled:        false, // opt-in: requires explicit enablement
					Model:          "claude-opus-4-6",
					TimeoutSeconds: 120, // two LLM passes (generate + risk classify) need headroom
				},
			},
			FeedbackReview: FeedbackReviewConfig{
				LLMProviderConfig: LLMProviderConfig{
					Enabled: false,
				},
			},
		},
		MCP: MCPConfig{
			Enabled:         true,
			ApprovalTimeout: 240,
			SessionTTL:      1440,
		},
		RuntimeProxy: RuntimeProxyConfig{
			Enabled:            false,
			ListenAddr:         "127.0.0.1:25290",
			DataDir:            "~/.clawvisor/runtime-proxy",
			TLS:                false,
			ListenerHostnames:  []string{"localhost", "127.0.0.1"},
			SessionTTLSeconds:  3600,
			TimingTraceEnabled: false,
			TimingTraceDir:     "~/.clawvisor/runtime-proxy/timing-traces",
		},
		RuntimePolicy: RuntimePolicyConfig{
			ObservationModeDefault:  false,
			InlineApprovalEnabled:   true,
			HarnessAllowlist:        nil,
			ToolLeaseTimeoutSeconds: 300,
			OneOffTTLSeconds:        300,
			AutovaultMode:           "observe",
			InjectStoredBearer:      false,
		},
		RateLimit: RateLimitConfig{
			Gateway:   RateLimitBucket{Limit: 60, Window: 60},
			OAuth:     RateLimitBucket{Limit: 5, Window: 60},
			PolicyAPI: RateLimitBucket{Limit: 30, Window: 60},
			ReviewRun: RateLimitBucket{Limit: 5, Window: 3600},
			Auth:      RateLimitBucket{Limit: 5, Window: 60},
		},
		Relay: RelayConfig{
			URL:                version.RelayURL(),
			KeyFile:            "daemon-ed25519.key",
			E2EKeyFile:         "daemon-x25519.key",
			ReconnectBaseDelay: "1s",
			ReconnectMaxDelay:  "60s",
			Enabled:            false,
		},
		Daemon: DaemonConfig{
			DataDir: "~/.clawvisor",
			LogFile: "logs/daemon.log",
		},
		Push: PushConfig{
			URL: version.PushURL(),
		},
		AutoUpdate: AutoUpdateConfig{
			Enabled:       false,
			CheckInterval: "6h",
		},
	}
}

// Load reads config from the given YAML path (optional) and applies env var overrides.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		if err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parsing config file: %w", err)
			}
		}
	}

	// Env overrides (12-factor friendly)
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.PostgresURL = v
	}
	if v := os.Getenv("DATABASE_DRIVER"); v != "" {
		cfg.Database.Driver = v
	}
	if v := os.Getenv("SQLITE_PATH"); v != "" {
		cfg.Database.SQLitePath = v
	}
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.Auth.JWTSecret = v
	}
	if v := os.Getenv("GCP_PROJECT"); v != "" {
		cfg.Vault.GCPProject = v
	}
	if v := os.Getenv("VAULT_BACKEND"); v != "" {
		cfg.Vault.Backend = v
	}
	if v := os.Getenv("VAULT_KEY_FILE"); v != "" {
		cfg.Vault.LocalKeyFile = v
	}
	if v := os.Getenv("VAULT_KEY"); v != "" {
		cfg.Vault.MasterKey = v
	}
	if v := os.Getenv("PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = port
		}
	}
	if v := os.Getenv("SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("PUBLIC_URL"); v != "" {
		cfg.Server.PublicURL = v
	} else if v := os.Getenv("RENDER_EXTERNAL_URL"); v != "" {
		cfg.Server.PublicURL = v
	}
	if v := os.Getenv("AUTH_MODE"); v != "" {
		cfg.Server.AuthMode = v
	}
	if v := os.Getenv("ALLOWED_EMAILS"); v != "" {
		cfg.Auth.AllowedEmails = strings.Split(v, ",")
	}
	if v := os.Getenv("MAX_USERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Auth.MaxUsers = n
		}
	}

	if v := os.Getenv("CALLBACK_ALLOW_PRIVATE_CIDRS"); v != "" {
		cfg.Callback.AllowPrivateCIDRs = strings.Split(v, ",")
	}
	if v := os.Getenv("CALLBACK_REQUIRE_HTTPS"); v != "" {
		cfg.Callback.RequireHTTPS = v == "true" || v == "1"
	}

	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Server.LogFormat = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Server.LogLevel = v
	}

	// Shared LLM overrides (inherited by all subsections)
	if v := os.Getenv("CLAWVISOR_LLM_PROVIDER"); v != "" {
		cfg.LLM.Provider = v
		if v == "vertex" && cfg.LLM.Endpoint == "https://api.anthropic.com/v1" {
			cfg.LLM.Endpoint = ""
		}
	}
	if v := os.Getenv("CLAWVISOR_LLM_ENDPOINT"); v != "" {
		cfg.LLM.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.LLM.TimeoutSeconds = n
		}
	}

	// Per-subsection overrides (take precedence over shared)
	if v := os.Getenv("CLAWVISOR_LLM_VERIFICATION_ENABLED"); v != "" {
		cfg.LLM.Verification.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_VERIFICATION_PROVIDER"); v != "" {
		cfg.LLM.Verification.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_VERIFICATION_ENDPOINT"); v != "" {
		cfg.LLM.Verification.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_VERIFICATION_API_KEY"); v != "" {
		cfg.LLM.Verification.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_VERIFICATION_MODEL"); v != "" {
		cfg.LLM.Verification.Model = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_VERIFICATION_FAIL_CLOSED"); v != "" {
		cfg.LLM.Verification.FailClosed = v == "true" || v == "1"
	}

	if v := os.Getenv("CLAWVISOR_LLM_TASK_RISK_ENABLED"); v != "" {
		cfg.LLM.TaskRisk.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_TASK_RISK_PROVIDER"); v != "" {
		cfg.LLM.TaskRisk.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_TASK_RISK_ENDPOINT"); v != "" {
		cfg.LLM.TaskRisk.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_TASK_RISK_API_KEY"); v != "" {
		cfg.LLM.TaskRisk.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_TASK_RISK_MODEL"); v != "" {
		cfg.LLM.TaskRisk.Model = v
	}

	if v := os.Getenv("CLAWVISOR_LLM_CHAIN_CONTEXT_ENABLED"); v != "" {
		cfg.LLM.ChainContext.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_CHAIN_CONTEXT_PROVIDER"); v != "" {
		cfg.LLM.ChainContext.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_CHAIN_CONTEXT_ENDPOINT"); v != "" {
		cfg.LLM.ChainContext.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_CHAIN_CONTEXT_API_KEY"); v != "" {
		cfg.LLM.ChainContext.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_CHAIN_CONTEXT_MODEL"); v != "" {
		cfg.LLM.ChainContext.Model = v
	}

	if v := os.Getenv("CLAWVISOR_LLM_ADAPTER_GEN_ENABLED"); v != "" {
		cfg.LLM.AdapterGen.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_ADAPTER_GEN_PROVIDER"); v != "" {
		cfg.LLM.AdapterGen.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_ADAPTER_GEN_ENDPOINT"); v != "" {
		cfg.LLM.AdapterGen.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_ADAPTER_GEN_API_KEY"); v != "" {
		cfg.LLM.AdapterGen.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_ADAPTER_GEN_MODEL"); v != "" {
		cfg.LLM.AdapterGen.Model = v
	}

	if v := os.Getenv("CLAWVISOR_LLM_FEEDBACK_REVIEW_ENABLED"); v != "" {
		cfg.LLM.FeedbackReview.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_FEEDBACK_REVIEW_PROVIDER"); v != "" {
		cfg.LLM.FeedbackReview.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_FEEDBACK_REVIEW_ENDPOINT"); v != "" {
		cfg.LLM.FeedbackReview.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_FEEDBACK_REVIEW_API_KEY"); v != "" {
		cfg.LLM.FeedbackReview.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_FEEDBACK_REVIEW_MODEL"); v != "" {
		cfg.LLM.FeedbackReview.Model = v
	}

	if v := os.Getenv("CLAWVISOR_NPS_SAMPLE_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
			cfg.Gateway.NPSSamplePercent = n
		}
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_ENABLED"); v != "" {
		cfg.RuntimeProxy.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_LISTEN_ADDR"); v != "" {
		cfg.RuntimeProxy.ListenAddr = v
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_DATA_DIR"); v != "" {
		cfg.RuntimeProxy.DataDir = v
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_TLS"); v != "" {
		cfg.RuntimeProxy.TLS = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_SESSION_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RuntimeProxy.SessionTTLSeconds = n
		}
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_TIMING_TRACE_ENABLED"); v != "" {
		cfg.RuntimeProxy.TimingTraceEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_PROXY_TIMING_TRACE_DIR"); v != "" {
		cfg.RuntimeProxy.TimingTraceDir = v
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_POLICY_OBSERVATION_DEFAULT"); v != "" {
		cfg.RuntimePolicy.ObservationModeDefault = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_POLICY_INLINE_APPROVAL_ENABLED"); v != "" {
		cfg.RuntimePolicy.InlineApprovalEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_POLICY_TOOL_LEASE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RuntimePolicy.ToolLeaseTimeoutSeconds = n
		}
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_POLICY_ONE_OFF_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RuntimePolicy.OneOffTTLSeconds = n
		}
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_POLICY_AUTOVAULT_MODE"); v != "" {
		cfg.RuntimePolicy.AutovaultMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("CLAWVISOR_RUNTIME_POLICY_INJECT_STORED_BEARER"); v != "" {
		cfg.RuntimePolicy.InjectStoredBearer = v == "true" || v == "1"
	}

	if v := os.Getenv("CLAWVISOR_RELAY_URL"); v != "" {
		cfg.Relay.URL = v
	}
	if v := os.Getenv("CLAWVISOR_RELAY_ENABLED"); v != "" {
		cfg.Relay.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_RELAY_DAEMON_ID"); v != "" {
		cfg.Relay.DaemonID = v
	}
	if v := os.Getenv("CLAWVISOR_RELAY_KEY_FILE"); v != "" {
		cfg.Relay.KeyFile = v
	}
	if v := os.Getenv("CLAWVISOR_RELAY_E2E_KEY_FILE"); v != "" {
		cfg.Relay.E2EKeyFile = v
	}
	if v := os.Getenv("CLAWVISOR_PUSH_ENABLED"); v != "" {
		cfg.Push.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_PUSH_URL"); v != "" {
		cfg.Push.URL = v
	}

	if v := os.Getenv("REDIS_URL"); v != "" {
		cfg.Redis.URL = v
	}

	if v := os.Getenv("CLAWVISOR_AUTO_UPDATE_ENABLED"); v != "" {
		cfg.AutoUpdate.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_AUTO_UPDATE_CHECK_INTERVAL"); v != "" {
		cfg.AutoUpdate.CheckInterval = v
	}

	if v := os.Getenv("CLAWVISOR_DAEMON_DATA_DIR"); v != "" {
		cfg.Daemon.DataDir = v
	}

	if v := os.Getenv("CLAWVISOR_TELEMETRY_ENABLED"); v != "" {
		cfg.Telemetry.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_TELEMETRY_ENDPOINT"); v != "" {
		cfg.Telemetry.Endpoint = v
	}

	// Inherit: fill empty subsection fields from shared LLM config.
	inheritLLMDefaults(&cfg.LLM.Verification.LLMProviderConfig, &cfg.LLM)
	inheritLLMDefaults(&cfg.LLM.TaskRisk.LLMProviderConfig, &cfg.LLM)
	inheritLLMDefaults(&cfg.LLM.ChainContext.LLMProviderConfig, &cfg.LLM)
	inheritLLMDefaults(&cfg.LLM.AdapterGen.LLMProviderConfig, &cfg.LLM)
	inheritLLMDefaults(&cfg.LLM.FeedbackReview.LLMProviderConfig, &cfg.LLM)

	// Resolve empty database driver: explicit env/config wins; otherwise auto-detect.
	if cfg.Database.Driver == "" {
		if cfg.Database.PostgresURL != "" {
			cfg.Database.Driver = "postgres"
		} else if cfg.Server.IsLocal() {
			cfg.Database.Driver = "sqlite"
			cfg.AutoConfig.DatabaseDriver = true
		} else {
			cfg.Database.Driver = "postgres"
		}
	}

	return cfg, nil
}

// inheritLLMDefaults fills empty fields in sub with the shared LLM-level defaults.
func inheritLLMDefaults(sub *LLMProviderConfig, shared *LLMConfig) {
	if sub.Provider == "" {
		sub.Provider = shared.Provider
	}
	if sub.Endpoint == "" {
		sub.Endpoint = shared.Endpoint
	}
	if sub.APIKey == "" {
		sub.APIKey = shared.APIKey
	}
	if sub.Model == "" {
		sub.Model = shared.Model
	}
	if sub.TimeoutSeconds == 0 {
		sub.TimeoutSeconds = shared.TimeoutSeconds
	}
}

// Validate checks for configuration errors that should prevent startup.
func (c *Config) Validate() error {
	if c.Database.Driver == "postgres" && c.Database.PostgresURL == "" {
		return fmt.Errorf("database driver is postgres but postgres_url is empty")
	}
	if c.Approval.Timeout <= 0 {
		return fmt.Errorf("approval.timeout must be positive (got %d)", c.Approval.Timeout)
	}
	if c.Task.DefaultExpirySeconds <= 0 {
		return fmt.Errorf("task.default_expiry_seconds must be positive (got %d)", c.Task.DefaultExpirySeconds)
	}
	if c.RuntimeProxy.Enabled && c.RuntimeProxy.ListenAddr == "" {
		return fmt.Errorf("runtime_proxy.listen_addr must be set when runtime_proxy.enabled is true")
	}
	if c.RuntimeProxy.SessionTTLSeconds <= 0 {
		return fmt.Errorf("runtime_proxy.session_ttl_seconds must be positive (got %d)", c.RuntimeProxy.SessionTTLSeconds)
	}
	if c.RuntimeProxy.TimingTraceEnabled && strings.TrimSpace(c.RuntimeProxy.TimingTraceDir) == "" {
		return fmt.Errorf("runtime_proxy.timing_trace_dir must be set when runtime_proxy.timing_trace_enabled is true")
	}
	if c.RuntimePolicy.ToolLeaseTimeoutSeconds <= 0 {
		return fmt.Errorf("runtime_policy.tool_lease_timeout_seconds must be positive (got %d)", c.RuntimePolicy.ToolLeaseTimeoutSeconds)
	}
	if c.RuntimePolicy.OneOffTTLSeconds <= 0 {
		return fmt.Errorf("runtime_policy.one_off_ttl_seconds must be positive (got %d)", c.RuntimePolicy.OneOffTTLSeconds)
	}
	switch strings.ToLower(strings.TrimSpace(c.RuntimePolicy.AutovaultMode)) {
	case "", "observe", "auto", "strict":
	default:
		return fmt.Errorf("runtime_policy.autovault_mode must be one of observe, auto, strict (got %q)", c.RuntimePolicy.AutovaultMode)
	}
	return nil
}

// AccessTokenDuration parses the configured duration.
func (a AuthConfig) AccessTokenDuration() (time.Duration, error) {
	return time.ParseDuration(a.AccessTokenTTL)
}

// RefreshTokenDuration parses the configured duration.
func (a AuthConfig) RefreshTokenDuration() (time.Duration, error) {
	return time.ParseDuration(a.RefreshTokenTTL)
}

// IsLocal returns true when the server is bound to a loopback address.
// Note: empty host and "0.0.0.0" bind all interfaces (including public ones)
// and are intentionally excluded.
func (s ServerConfig) IsLocal() bool {
	return s.Host == "localhost" || s.Host == "127.0.0.1"
}

// Addr returns the server listen address.
func (s ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// SlogLevel returns the slog.Level matching the configured LogLevel string.
func (s ServerConfig) SlogLevel() slog.Level {
	switch strings.ToLower(s.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
