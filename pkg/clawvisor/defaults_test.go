package clawvisor

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/config"
)

func TestRuntimePolicySurfaceEnabledByProxyLite(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = false
	cfg.ProxyLite.Enabled = true

	if !runtimePolicySurfaceEnabled(cfg) {
		t.Fatalf("proxy-lite should expose runtime policy surfaces")
	}
}

func TestRuntimePolicySurfaceDisabledWithoutRuntimeSurfaces(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = false
	cfg.ProxyLite.Enabled = false

	if runtimePolicySurfaceEnabled(cfg) {
		t.Fatalf("runtime policy surface should be hidden when both runtime proxy and proxy-lite are disabled")
	}
}
