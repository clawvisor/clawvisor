package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Vault    VaultConfig    `yaml:"vault"`
	Auth     AuthConfig     `yaml:"auth"`
	Approval ApprovalConfig `yaml:"approval"`
	Task     TaskConfig     `yaml:"task"`
	LLM      LLMConfig      `yaml:"llm"`
	MCP      MCPConfig      `yaml:"mcp"`
	Telegram TelegramConfig `yaml:"telegram"`
	Google   GoogleConfig   `yaml:"google"`
}

// TaskConfig holds settings for task-scoped authorization.
type TaskConfig struct {
	DefaultExpirySeconds int `yaml:"default_expiry_seconds"` // default: 1800 (30 min)
}

type ServerConfig struct {
	Port        int    `yaml:"port"`
	Host        string `yaml:"host"`
	FrontendDir string `yaml:"frontend_dir"`
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
}

type AuthConfig struct {
	JWTSecret       string `yaml:"jwt_secret"`
	AccessTokenTTL  string `yaml:"access_token_ttl"`
	RefreshTokenTTL string `yaml:"refresh_token_ttl"`
}

type ApprovalConfig struct {
	Timeout   int    `yaml:"timeout"`
	OnTimeout string `yaml:"on_timeout"`
}

// LLMProviderConfig holds settings for one LLM provider endpoint.
// Used for safety checker, response filters, and policy authoring.
type LLMProviderConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Provider       string `yaml:"provider"`        // "openai" (default) | "anthropic"
	Endpoint       string `yaml:"endpoint"`        // Base URL, e.g. https://api.anthropic.com/v1
	APIKey         string `yaml:"api_key"`          // Overridable via env
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	SkipReadonly   bool   `yaml:"skip_readonly"` // Safety only: skip check for read actions
}

// LLMConfig groups all LLM provider configurations.
type LLMConfig struct {
	Safety   LLMProviderConfig `yaml:"safety"`   // Post-policy safety checker (runtime)
	Filters  LLMProviderConfig `yaml:"filters"`  // Semantic response filters (runtime)
	Authoring LLMProviderConfig `yaml:"authoring"` // Policy authoring assistant (interactive only)
}

type MCPConfig struct {
	ApprovalTimeout int `yaml:"approval_timeout"`
}

// TelegramConfig holds the Telegram bot token used for approval notifications.
// The per-user chat ID is stored in notification_configs (channel="telegram").
type TelegramConfig struct {
	BotToken string `yaml:"bot_token"`
}

// GoogleConfig holds OAuth2 credentials for all Google adapters.
type GoogleConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RedirectURL  string `yaml:"redirect_url"`
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port:        8080,
			Host:        "127.0.0.1",
			FrontendDir: "./web/dist",
		},
		Database: DatabaseConfig{
			Driver:     "postgres",
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
			Safety: LLMProviderConfig{
				Enabled:        false,
				Provider:       "openai",
				Endpoint:       "https://api.openai.com/v1",
				Model:          "gpt-4o-mini",
				TimeoutSeconds: 10,
				SkipReadonly:   true,
			},
			Filters: LLMProviderConfig{
				Enabled:        false,
				Provider:       "openai",
				Endpoint:       "https://api.openai.com/v1",
				Model:          "gpt-4o-mini",
				TimeoutSeconds: 10,
			},
			Authoring: LLMProviderConfig{
				Enabled:        false,
				Provider:       "anthropic",
				Endpoint:       "https://api.anthropic.com/v1",
				Model:          "claude-haiku-4-5-20251001",
				TimeoutSeconds: 30,
			},
		},
		MCP: MCPConfig{
			ApprovalTimeout: 240,
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
	if v := os.Getenv("PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = port
		}
	}
	if v := os.Getenv("SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.Telegram.BotToken = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_ID"); v != "" {
		cfg.Google.ClientID = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.Google.ClientSecret = v
	}
	if v := os.Getenv("GOOGLE_REDIRECT_URL"); v != "" {
		cfg.Google.RedirectURL = v
	}

	// LLM Safety overrides
	if v := os.Getenv("CLAWVISOR_LLM_SAFETY_ENABLED"); v != "" {
		cfg.LLM.Safety.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_SAFETY_PROVIDER"); v != "" {
		cfg.LLM.Safety.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_SAFETY_ENDPOINT"); v != "" {
		cfg.LLM.Safety.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_SAFETY_API_KEY"); v != "" {
		cfg.LLM.Safety.APIKey = v
	}
	// Backward compat with old env var name
	if v := os.Getenv("SAFETY_LLM_API_KEY"); v != "" && cfg.LLM.Safety.APIKey == "" {
		cfg.LLM.Safety.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_SAFETY_MODEL"); v != "" {
		cfg.LLM.Safety.Model = v
	}

	// LLM Filters overrides
	if v := os.Getenv("CLAWVISOR_LLM_FILTERS_ENABLED"); v != "" {
		cfg.LLM.Filters.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_FILTERS_PROVIDER"); v != "" {
		cfg.LLM.Filters.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_FILTERS_ENDPOINT"); v != "" {
		cfg.LLM.Filters.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_FILTERS_API_KEY"); v != "" {
		cfg.LLM.Filters.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_FILTERS_MODEL"); v != "" {
		cfg.LLM.Filters.Model = v
	}

	// LLM Authoring overrides
	if v := os.Getenv("CLAWVISOR_LLM_AUTHORING_ENABLED"); v != "" {
		cfg.LLM.Authoring.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CLAWVISOR_LLM_AUTHORING_PROVIDER"); v != "" {
		cfg.LLM.Authoring.Provider = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_AUTHORING_ENDPOINT"); v != "" {
		cfg.LLM.Authoring.Endpoint = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_AUTHORING_API_KEY"); v != "" {
		cfg.LLM.Authoring.APIKey = v
	}
	if v := os.Getenv("CLAWVISOR_LLM_AUTHORING_MODEL"); v != "" {
		cfg.LLM.Authoring.Model = v
	}

	return cfg, nil
}

// AccessTokenTTL parses the configured duration.
func (a AuthConfig) AccessTokenDuration() (time.Duration, error) {
	return time.ParseDuration(a.AccessTokenTTL)
}

// RefreshTokenDuration parses the configured duration.
func (a AuthConfig) RefreshTokenDuration() (time.Duration, error) {
	return time.ParseDuration(a.RefreshTokenTTL)
}

// Addr returns the server listen address.
func (s ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

