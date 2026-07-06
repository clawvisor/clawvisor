package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cfgpkg "github.com/clawvisor/clawvisor/pkg/config"
)

// TestFlipWizardDefaultsToObserve locks the writer-side flip (spec 08 / PRD
// §11): the wizard's recommended, pre-selected posture is Observe, so a fresh
// install that accepts the defaults lands in the Observe posture with an
// explicit proxy_lite.enabled: true. Skill-gateway-only is the opt-out. This
// is the wizard half of TestFlipFreshWizardLandsObserve (the boot half lives
// in e2e/scenarios); together they prove the fresh default flipped without
// touching Default() (see TestFlipDefaultStaysFalse in pkg/config).
func TestFlipWizardDefaultsToObserve(t *testing.T) {
	if got := recommendedPosture(); got != "observe" {
		t.Fatalf("recommendedPosture()=%q, want observe (fresh-install flip default)", got)
	}
}

// TestWizardWritesMarkerAndExplicitKey asserts writeConfig always stamps the
// config_schema marker and an explicit proxy_lite.enabled key, and that a
// wizard config round-trips through Load without silently enabling proxy-lite.
func TestWizardWritesMarkerAndExplicitKey(t *testing.T) {
	t.Run("skill gateway default", func(t *testing.T) {
		cfg := &config{
			envMode:          "local",
			host:             "127.0.0.1",
			port:             "25297",
			vault:            "local",
			llmProvider:      "anthropic",
			llmEndpoint:      "https://api.anthropic.com/v1",
			llmModel:         "claude-haiku-4-5",
			posture:          "",
			proxyLiteEnabled: false,
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := writeConfig(cfg, path); err != nil {
			t.Fatalf("writeConfig: %v", err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		text := string(body)
		if !strings.Contains(text, "config_schema: 2") {
			t.Fatalf("missing config_schema marker:\n%s", text)
		}
		if !strings.Contains(text, "proxy_lite:") || !strings.Contains(text, "enabled: false") {
			t.Fatalf("missing explicit proxy_lite.enabled: false:\n%s", text)
		}
		if strings.Contains(text, "posture:") {
			t.Fatalf("skill-gateway config must not write a posture key:\n%s", text)
		}
		// Round-trip: loading it must NOT enable proxy-lite.
		loaded, err := cfgpkg.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if loaded.ProxyLite.Enabled {
			t.Fatal("skill-gateway wizard config must not enable proxy-lite")
		}
		if loaded.ConfigSchema != cfgpkg.CurrentConfigSchema {
			t.Fatalf("ConfigSchema=%d, want %d", loaded.ConfigSchema, cfgpkg.CurrentConfigSchema)
		}
	})

	t.Run("observe posture", func(t *testing.T) {
		cfg := &config{
			envMode:            "local",
			host:               "127.0.0.1",
			port:               "25297",
			vault:              "local",
			llmProvider:        "anthropic",
			llmEndpoint:        "https://api.anthropic.com/v1",
			llmModel:           "claude-haiku-4-5",
			posture:            "observe",
			proxyLiteEnabled:   true,
			proxyLitePublicURL: "http://127.0.0.1:25297",
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := writeConfig(cfg, path); err != nil {
			t.Fatalf("writeConfig: %v", err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		text := string(body)
		if !strings.Contains(text, "posture: observe") {
			t.Fatalf("missing posture: observe:\n%s", text)
		}
		if !strings.Contains(text, "enabled: true") {
			t.Fatalf("observe config must write proxy_lite.enabled: true:\n%s", text)
		}
		loaded, err := cfgpkg.Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !loaded.ProxyLite.Enabled || !loaded.ProxyLite.ObserveMode() || !loaded.ProxyLite.PassthroughUpstreamAuth() {
			t.Fatalf("observe wizard config should yield enabled+observe+passthrough: enabled=%v observe=%v passthrough=%v",
				loaded.ProxyLite.Enabled, loaded.ProxyLite.ObserveMode(), loaded.ProxyLite.PassthroughUpstreamAuth())
		}
	})
}
