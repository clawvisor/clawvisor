package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AutoConfigured tracks which settings were auto-resolved (not explicitly set).
type AutoConfigured struct {
	DatabaseDriver bool
	JWTSecret      bool
}

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Vault     VaultConfig     `yaml:"vault"`
	Auth      AuthConfig      `yaml:"auth"`
	Approval  ApprovalConfig  `yaml:"approval"`
	Callback  CallbackConfig  `yaml:"callback"`
	Task      TaskConfig      `yaml:"task"`
	LLM       LLMConfig       `yaml:"llm"`
	MCP       MCPConfig       `yaml:"mcp"`
	Services  ServicesConfig  `yaml:"services"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`

	AutoConfig AutoConfigured `yaml:"-"`
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
	PublicURL   string `yaml:"public_url"`  // e.g. "http://192.168.4.247:5173" — used in Telegram notification links
	AuthMode    string `yaml:"auth_mode"`   // "magic_link", "password", or "" (auto-detect from IsLocal)
	LogFormat   string `yaml:"log_format"`  // "json", "text", or "" (auto: json in prod, text in dev)
	LogLevel    string `yaml:"log_level"`   // "debug", "info", "warn", "error" (default: "info")
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
}

type ApprovalConfig struct {
	Timeout   int    `yaml:"timeout"`
	OnTimeout string `yaml:"on_timeout"` // Reserved for Phase 9: behavior when approval times out ("fail" or "allow")
}

// LLMProviderConfig holds settings for one LLM provider endpoint.
type LLMProviderConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Provider       string `yaml:"provider"`        // "openai" (default) | "anthropic"
	Endpoint       string `yaml:"endpoint"`        // Base URL, e.g. https://api.anthropic.com/v1
	APIKey         string `yaml:"api_key"`          // Overridable via env
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

// LLMConfig groups all LLM provider configurations.
type LLMConfig struct {
	Verification VerificationConfig `yaml:"verification"` // Intent verification (runtime)
}

// MCPConfig holds settings for the MCP server.
type MCPConfig struct {
	Enabled         bool `yaml:"enabled"`          // default: true
	ApprovalTimeout int  `yaml:"approval_timeout"` // MCP tool call approval timeout in seconds (default 240s)
	SessionTTL      int  `yaml:"session_ttl"`      // session TTL in minutes (default: 1440 = 24h)
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
}

// ServicesConfig groups all adapter/service-specific settings.
type ServicesConfig struct {
	Google   GoogleServicesConfig   `yaml:"google"`
	GitHub   GitHubServicesConfig   `yaml:"github"`
	IMessage IMessageServicesConfig `yaml:"imessage"`
	Slack    SlackServicesConfig    `yaml:"slack"`
	Notion   NotionServicesConfig   `yaml:"notion"`
	Linear   LinearServicesConfig   `yaml:"linear"`
	Stripe   StripeServicesConfig   `yaml:"stripe"`
	Twilio   TwilioServicesConfig   `yaml:"twilio"`
}

// GoogleServicesConfig holds OAuth2 credentials for all Google adapters.
type GoogleServicesConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RedirectURL  string `yaml:"redirect_url"`
}

// GitHubServicesConfig holds settings for the GitHub adapter.
type GitHubServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

// IMessageServicesConfig holds settings for the iMessage adapter.
type IMessageServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

// SlackServicesConfig holds settings for the Slack adapter.
type SlackServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

// NotionServicesConfig holds settings for the Notion adapter.
type NotionServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

// LinearServicesConfig holds settings for the Linear adapter.
type LinearServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

// StripeServicesConfig holds settings for the Stripe adapter.
type StripeServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

// TwilioServicesConfig holds settings for the Twilio adapter.
type TwilioServicesConfig struct {
	Enabled bool `yaml:"enabled"`
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port:        25297,
			Host:        "127.0.0.1",
			FrontendDir: "./web/dist",
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
		LLM: LLMConfig{
			Verification: VerificationConfig{
				LLMProviderConfig: LLMProviderConfig{
					Enabled:        false,
					Provider:       "anthropic",
					Endpoint:       "https://api.anthropic.com/v1",
					Model:          "claude-haiku-4-5-20251001",
					TimeoutSeconds: 5,
				},
				FailClosed:      true,
				CacheTTLSeconds: 60,
			},
		},
		MCP: MCPConfig{
			Enabled:         true,
			ApprovalTimeout: 240,
			SessionTTL:      1440,
		},
		RateLimit: RateLimitConfig{
			Gateway:   RateLimitBucket{Limit: 60, Window: 60},
			OAuth:     RateLimitBucket{Limit: 5, Window: 60},
			PolicyAPI: RateLimitBucket{Limit: 30, Window: 60},
			ReviewRun: RateLimitBucket{Limit: 5, Window: 3600},
		},
		Services: ServicesConfig{
			GitHub:   GitHubServicesConfig{Enabled: true},
			IMessage: IMessageServicesConfig{Enabled: true},
			Slack:    SlackServicesConfig{Enabled: true},
			Notion:   NotionServicesConfig{Enabled: true},
			Linear:   LinearServicesConfig{Enabled: true},
			Stripe:   StripeServicesConfig{Enabled: true},
			Twilio:   TwilioServicesConfig{Enabled: true},
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
	}
	if v := os.Getenv("AUTH_MODE"); v != "" {
		cfg.Server.AuthMode = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_ID"); v != "" {
		cfg.Services.Google.ClientID = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.Services.Google.ClientSecret = v
	}
	if v := os.Getenv("GOOGLE_REDIRECT_URL"); v != "" {
		cfg.Services.Google.RedirectURL = v
	}
	if v := os.Getenv("GITHUB_ENABLED"); v != "" {
		cfg.Services.GitHub.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("IMESSAGE_ENABLED"); v != "" {
		cfg.Services.IMessage.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("SLACK_ENABLED"); v != "" {
		cfg.Services.Slack.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("NOTION_ENABLED"); v != "" {
		cfg.Services.Notion.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LINEAR_ENABLED"); v != "" {
		cfg.Services.Linear.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("STRIPE_ENABLED"); v != "" {
		cfg.Services.Stripe.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("TWILIO_ENABLED"); v != "" {
		cfg.Services.Twilio.Enabled = v == "true" || v == "1"
	}

	if v := os.Getenv("ALLOWED_EMAILS"); v != "" {
		cfg.Auth.AllowedEmails = strings.Split(v, ",")
	}

	if v := os.Getenv("CALLBACK_ALLOW_PRIVATE_CIDRS"); v != "" {
		cfg.Callback.AllowPrivateCIDRs = strings.Split(v, ",")
	}
	if v := os.Getenv("CALLBACK_REQUIRE_HTTPS"); v != "" {
		cfg.Callback.RequireHTTPS = v == "true" || v == "1"
	}

	// LLM Verification overrides
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Server.LogFormat = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Server.LogLevel = v
	}

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

// AccessTokenDuration parses the configured duration.
func (a AuthConfig) AccessTokenDuration() (time.Duration, error) {
	return time.ParseDuration(a.AccessTokenTTL)
}

// RefreshTokenDuration parses the configured duration.
func (a AuthConfig) RefreshTokenDuration() (time.Duration, error) {
	return time.ParseDuration(a.RefreshTokenTTL)
}

// IsLocal returns true when the server is bound to a loopback or wildcard address.
func (s ServerConfig) IsLocal() bool {
	return s.Host == "" || s.Host == "localhost" || s.Host == "127.0.0.1" || s.Host == "0.0.0.0"
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
