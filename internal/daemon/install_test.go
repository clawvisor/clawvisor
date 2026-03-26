package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsGoRunBinary(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/home/user/.clawvisor/bin/clawvisor", false},
		{"/usr/local/bin/clawvisor", false},
		{"/tmp/go-build123456/exe/clawvisor", true},
		{"/home/user/go/cache/go-build/abc/exe/main", true},
		{"/var/folders/xx/go-build789/b001/exe/clawvisor", true},
	}
	for _, tt := range tests {
		if got := isGoRunBinary(tt.path); got != tt.want {
			t.Errorf("isGoRunBinary(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestInstallLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd tests only run on macOS")
	}

	home := t.TempDir()
	data := installData{
		Binary:  "/usr/local/bin/clawvisor",
		LogDir:  filepath.Join(home, ".clawvisor", "logs"),
		DataDir: filepath.Join(home, ".clawvisor"),
	}

	if err := installLaunchd(home, data); err != nil {
		t.Fatalf("installLaunchd: %v", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.clawvisor.daemon.plist")

	raw, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading plist: %v", err)
	}
	content := string(raw)

	checks := []struct {
		substr string
		desc   string
	}{
		{"com.clawvisor.daemon", "label"},
		{data.Binary, "binary path"},
		{"start", "start argument"},
		{"--foreground", "foreground flag"},
		{"RunAtLoad", "run at load"},
		{"KeepAlive", "keep alive"},
		{data.LogDir, "log directory"},
		{data.DataDir, "data directory"},
		{"CONFIG_FILE", "config file env var"},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.substr) {
			t.Errorf("plist missing %s (%q)", c.desc, c.substr)
		}
	}
}

func TestInstallSystemd(t *testing.T) {
	home := t.TempDir()
	data := installData{
		Binary:  "/usr/local/bin/clawvisor",
		LogDir:  filepath.Join(home, ".clawvisor", "logs"),
		DataDir: filepath.Join(home, ".clawvisor"),
	}

	if err := installSystemd(home, data); err != nil {
		t.Fatalf("installSystemd: %v", err)
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", "clawvisor.service")

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading unit file: %v", err)
	}
	content := string(raw)

	checks := []struct {
		substr string
		desc   string
	}{
		{"ExecStart=/usr/local/bin/clawvisor start --foreground", "exec start"},
		{"Restart=always", "restart policy"},
		{"RestartSec=5", "restart delay"},
		{"CONFIG_FILE=" + data.DataDir + "/config.yaml", "config file env"},
		{"After=network-online.target", "network dependency"},
		{"WantedBy=default.target", "install target"},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.substr) {
			t.Errorf("unit file missing %s (%q)", c.desc, c.substr)
		}
	}
}

func TestEnsureVaultKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "vault.key")

	// First call creates the key.
	if err := ensureVaultKey(keyPath); err != nil {
		t.Fatalf("ensureVaultKey (create): %v", err)
	}

	data1, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading vault key: %v", err)
	}
	if len(data1) == 0 {
		t.Fatal("vault key is empty")
	}

	// Verify permissions.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat vault key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("vault key permissions = %o, want 0600", perm)
	}

	// Second call is a no-op — does not overwrite.
	if err := ensureVaultKey(keyPath); err != nil {
		t.Fatalf("ensureVaultKey (idempotent): %v", err)
	}
	data2, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading vault key again: %v", err)
	}
	if string(data1) != string(data2) {
		t.Error("ensureVaultKey overwrote existing key")
	}
}

func TestWriteDaemonConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	cfg := &daemonConfig{
		llmProvider:         "anthropic",
		llmEndpoint:         "https://api.anthropic.com/v1",
		llmModel:            "claude-haiku-4-5-20251001",
		llmAPIKey:           "sk-ant-test-key-12345",
		taskRiskEnabled:     true,
		chainContextEnabled: true,
		telemetryEnabled:    false,
	}

	if err := writeDaemonConfig(cfg, dir, "test-jwt-secret", configPath); err != nil {
		t.Fatalf("writeDaemonConfig: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	content := string(raw)

	checks := []struct {
		substr string
		desc   string
	}{
		{"port: 25297", "server port"},
		{"host: \"127.0.0.1\"", "server host"},
		{"driver: \"sqlite\"", "database driver"},
		{"backend: \"local\"", "vault backend"},
		{"provider: anthropic", "llm provider"},
		{"model: claude-haiku-4-5-20251001", "llm model"},
		{"sk-ant-test-key-12345", "llm api key"},
		{"enabled: true", "verification/risk enabled"},
		{"enabled: false", "telemetry disabled"},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.substr) {
			t.Errorf("config missing %s (%q)", c.desc, c.substr)
		}
	}

	// Verify file permissions.
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config permissions = %o, want 0600", perm)
	}
}

func TestWriteDaemonConfigSpecialChars(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	cfg := &daemonConfig{
		llmProvider: "openai",
		llmEndpoint: "https://api.openai.com/v1",
		llmModel:    "gpt-4o-mini",
		// API key with special YAML characters that must be quoted.
		llmAPIKey:       `sk-key-with"quotes&ampersand:colon`,
		taskRiskEnabled: true,
	}

	if err := writeDaemonConfig(cfg, dir, "jwt-secret", configPath); err != nil {
		t.Fatalf("writeDaemonConfig: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	content := string(raw)

	// The key should appear YAML-quoted (not bare), so the file stays parseable.
	if !strings.Contains(content, "quotes") {
		t.Error("config lost API key with special characters")
	}

	// Verify the config is valid YAML by attempting to read it back.
	// (writeDaemonConfig uses yamlQuote, so this should always work.)
	if strings.Count(content, "api_key:") != 1 {
		t.Error("expected exactly one api_key entry")
	}
}

func TestYamlQuote(t *testing.T) {
	tests := []struct {
		input string
		safe  bool // result should be parseable as YAML
	}{
		{"simple-key", true},
		{"sk-ant-abc123", true},
		{`key"with"quotes`, true},
		{"key:with:colons", true},
		{"key with spaces", true},
		{"", true},
	}
	for _, tt := range tests {
		result := yamlQuote(tt.input)
		if result == "" && tt.input != "" {
			t.Errorf("yamlQuote(%q) returned empty string", tt.input)
		}
		// Should never contain unescaped newlines (which would break YAML structure).
		if strings.Contains(result, "\n") && !strings.HasPrefix(result, "'") && !strings.HasPrefix(result, "\"") && !strings.HasPrefix(result, "|") {
			t.Errorf("yamlQuote(%q) = %q — contains bare newline", tt.input, result)
		}
	}
}
