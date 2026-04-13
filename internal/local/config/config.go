package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the daemon-level configuration from config.yaml.
type Config struct {
	Name                 string            `yaml:"name"`
	Port                 int               `yaml:"port"`
	LogLevel             string            `yaml:"log_level"`
	ServiceDirs          []string          `yaml:"service_dirs"`
	ScanInterval         int               `yaml:"scan_interval"`
	DefaultTimeout       Duration          `yaml:"default_timeout"`
	MaxOutputSize        int64             `yaml:"max_output_size"`
	MaxConcurrentReqs    int               `yaml:"max_concurrent_requests"`
	Env                  map[string]string `yaml:"env"`
	AllowedCloudOrigins  []string          `yaml:"allowed_cloud_origins"`
}

// Duration wraps time.Duration for YAML unmarshalling of strings like "30s".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// DefaultConfig returns the config with all defaults applied.
func DefaultConfig(baseDir string) *Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	return &Config{
		Name:                hostname,
		Port:                25299,
		LogLevel:            "info",
		ServiceDirs:         []string{filepath.Join(baseDir, "services")},
		ScanInterval:        0,
		DefaultTimeout:      Duration{30 * time.Second},
		MaxOutputSize:       1048576, // 1 MB
		MaxConcurrentReqs:   10,
		Env:                 make(map[string]string),
		AllowedCloudOrigins: []string{"https://app.clawvisor.com"},
	}
}

// Load reads config.yaml from the given base directory.
// If the file doesn't exist, returns defaults.
func Load(baseDir string) (*Config, error) {
	cfg := DefaultConfig(baseDir)

	path := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config.yaml: %w", err)
	}

	// Expand tildes in service dirs.
	home, _ := os.UserHomeDir()
	for i, dir := range cfg.ServiceDirs {
		if len(dir) > 0 && dir[0] == '~' {
			cfg.ServiceDirs[i] = filepath.Join(home, dir[1:])
		}
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid port: %d", cfg.Port)
	}
	if cfg.MaxConcurrentReqs < 1 {
		cfg.MaxConcurrentReqs = 10
	}
	if cfg.MaxOutputSize < 1 {
		cfg.MaxOutputSize = 1048576
	}

	return cfg, nil
}

// BaseDir returns the default base directory (~/.clawvisor/local).
func BaseDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".clawvisor", "local")
}
