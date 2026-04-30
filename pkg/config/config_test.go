package config

import (
	"strings"
	"testing"
)

func TestLoadAppliesRuntimeProxyTimingTraceEnv(t *testing.T) {
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_TIMING_TRACE_ENABLED", "true")
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_TIMING_TRACE_DIR", "/tmp/clawvisor-timing-traces")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RuntimeProxy.TimingTraceEnabled {
		t.Fatal("expected timing trace env override to enable runtime proxy timing traces")
	}
	if cfg.RuntimeProxy.TimingTraceDir != "/tmp/clawvisor-timing-traces" {
		t.Fatalf("expected timing trace dir override, got %q", cfg.RuntimeProxy.TimingTraceDir)
	}
}

func TestValidateRequiresTimingTraceDirWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.RuntimeProxy.TimingTraceEnabled = true
	cfg.RuntimeProxy.TimingTraceDir = "   "

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "runtime_proxy.timing_trace_dir") {
		t.Fatalf("expected timing trace dir validation error, got %v", err)
	}
}
