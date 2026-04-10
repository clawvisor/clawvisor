package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// AutoUpdateStatus reads config.yaml and returns the auto-update settings.
func AutoUpdateStatus() (enabled bool, checkInterval string, err error) {
	dataDir, err := ensureDataDir()
	if err != nil {
		return false, "", err
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false, "", fmt.Errorf("reading config: %w", err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false, "", fmt.Errorf("parsing config: %w", err)
	}

	au, ok := raw["auto_update"].(map[string]interface{})
	if !ok {
		return false, "6h", nil
	}
	if e, ok := au["enabled"].(bool); ok {
		enabled = e
	}
	interval := "6h"
	if ci, ok := au["check_interval"].(string); ok && ci != "" {
		interval = ci
	}
	return enabled, interval, nil
}

// SetAutoUpdate patches config.yaml to enable or disable auto-update.
// Uses line-level editing to avoid corrupting other values via YAML round-trip.
func SetAutoUpdate(enable bool) error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	cfgPath := filepath.Join(dataDir, "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	content := string(data)
	enabledLine := fmt.Sprintf("  enabled: %t", enable)

	if strings.Contains(content, "auto_update:") {
		// Section exists — find and replace the enabled line.
		lines := strings.Split(content, "\n")
		inSection := false
		replaced := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			// Detect section headers (top-level keys with no indentation).
			if len(line) > 0 && line[0] != ' ' && line[0] != '#' && strings.HasSuffix(trimmed, ":") {
				inSection = trimmed == "auto_update:"
			}
			if inSection && strings.HasPrefix(trimmed, "enabled:") {
				lines[i] = enabledLine
				replaced = true
				break
			}
		}
		if !replaced {
			// Section exists but no enabled key — insert after the header.
			content = strings.Replace(content, "auto_update:\n", "auto_update:\n"+enabledLine+"\n", 1)
		} else {
			content = strings.Join(lines, "\n")
		}
	} else {
		// No auto_update section — append one.
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "\nauto_update:\n" + enabledLine + "\n  check_interval: \"6h\"\n"
	}

	return os.WriteFile(cfgPath, []byte(content), 0600)
}
