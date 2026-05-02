package config

import (
	"strings"
	"testing"
)

func TestLoadAppliesRuntimeProxyTimingTraceEnv(t *testing.T) {
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_TIMING_TRACE_ENABLED", "true")
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_TIMING_TRACE_DIR", "/tmp/clawvisor-timing-traces")
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_BODY_TRACE_ENABLED", "true")
	t.Setenv("CLAWVISOR_RUNTIME_PROXY_BODY_TRACE_DIR", "/tmp/clawvisor-body-traces")

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
	if !cfg.RuntimeProxy.BodyTraceEnabled {
		t.Fatal("expected body trace env override to enable runtime proxy body traces")
	}
	if cfg.RuntimeProxy.BodyTraceDir != "/tmp/clawvisor-body-traces" {
		t.Fatalf("expected body trace dir override, got %q", cfg.RuntimeProxy.BodyTraceDir)
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

func TestValidateRequiresBodyTraceDirWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.RuntimeProxy.BodyTraceEnabled = true
	cfg.RuntimeProxy.BodyTraceDir = "   "

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "runtime_proxy.body_trace_dir") {
		t.Fatalf("expected body trace dir validation error, got %v", err)
	}
}
