package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Profile holds connection details for a Clawvisor server.
type Profile struct {
	ServerURL string `yaml:"server_url"`
	Token     string `yaml:"token"`
}

// Config is the TUI configuration file (~/.clawvisor/tui.yaml).
type Config struct {
	Profiles      map[string]Profile `yaml:"profiles"`
	ActiveProfile string             `yaml:"active_profile"`
	PollInterval  int                `yaml:"poll_interval"`
}

// DefaultDir returns ~/.clawvisor.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".clawvisor"), nil
}

// DefaultPath returns ~/.clawvisor/tui.yaml.
func DefaultPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tui.yaml"), nil
}

// Load reads the config file at path. Returns an error if the file doesn't exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5
	}
	if cfg.ActiveProfile == "" {
		cfg.ActiveProfile = "default"
	}
	return &cfg, nil
}

// Active returns the active profile. Returns an error if it doesn't exist.
func (c *Config) Active() (Profile, error) {
	p, ok := c.Profiles[c.ActiveProfile]
	if !ok {
		return Profile{}, fmt.Errorf("profile %q not found", c.ActiveProfile)
	}
	return p, nil
}

// Save writes the config to path, creating parent directories as needed.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
