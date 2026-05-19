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

// Regression: enabling proxy_lite alone must surface the Shadow
// Tokens UI. Pre-fix the gate AND-required runtime_proxy.Enabled,
// so a pure proxy-lite install never saw the vault panel even
// though the resolver consumes autovault placeholders.
func TestComputeFeatureSet_SecretVaultUnderProxyLite(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = false
	cfg.ProxyLite.Enabled = true
	cfg.Features.SecretVault = false // explicit opt-in NOT set

	got := computeFeatureSet(cfg)
	if !got.SecretVault {
		t.Errorf("SecretVault should be true when proxy_lite is enabled, even without features.secret_vault")
	}
	if !got.ProxyLite {
		t.Errorf("ProxyLite should expose the proxy_lite feature flag")
	}
	if !got.AgentLiveSessions {
		t.Errorf("AgentLiveSessions should be true when proxy_lite is enabled")
	}
}

func TestComputeFeatureSet_SecretVaultHiddenUnderRuntimeProxyWithoutFlag(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.ProxyLite.Enabled = false
	cfg.Features.SecretVault = false

	got := computeFeatureSet(cfg)
	if got.SecretVault {
		t.Errorf("SecretVault should stay hidden under runtime_proxy unless features.secret_vault is enabled")
	}
	if got.ProxyLite {
		t.Errorf("ProxyLite should be false when proxy_lite is disabled")
	}
}

func TestComputeFeatureSet_SecretVaultUnderRuntimeProxyWithFlag(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.ProxyLite.Enabled = false
	cfg.Features.SecretVault = true

	got := computeFeatureSet(cfg)
	if !got.SecretVault {
		t.Errorf("SecretVault should follow the main-branch runtime_proxy + features.secret_vault gate")
	}
}

func TestComputeFeatureSet_SecretVaultExplicitOptInWithoutRuntimeProxy(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = false
	cfg.ProxyLite.Enabled = false
	cfg.Features.SecretVault = true

	got := computeFeatureSet(cfg)
	if got.SecretVault {
		t.Errorf("SecretVault should not be enabled by features.secret_vault alone when proxy_lite is disabled")
	}
}

func TestComputeFeatureSet_SecretVaultHiddenWithoutAnyProxy(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = false
	cfg.ProxyLite.Enabled = false
	cfg.Features.SecretVault = false

	got := computeFeatureSet(cfg)
	if got.SecretVault {
		t.Errorf("SecretVault should be hidden when no proxy is enabled and no explicit opt-in")
	}
}
