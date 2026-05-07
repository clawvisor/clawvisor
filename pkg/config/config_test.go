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

// TestInheritLLMDefaults_DoesNotInheritAnthropicEndpointIntoNonAnthropicSubBlock
// covers two cases where the Anthropic-default endpoint must NOT propagate
// into a sub-block that runs on a different provider:
//
//  1. Top-level switches to gemini (sub-block inherits provider).
//  2. Mixed providers: top-level stays anthropic, sub-block overrides to gemini.
//
// In both cases the sub-block's Endpoint must end up empty so the
// per-provider URL builder in llm.NewClient kicks in. Without this, gemini
// requests POST to api.anthropic.com and Cloudflare 404s.
func TestInheritLLMDefaults_DoesNotInheritAnthropicEndpointIntoNonAnthropicSubBlock(t *testing.T) {
	const anthropicURL = "https://api.anthropic.com/v1"

	// Case 1: top-level provider=gemini, sub-blocks inherit.
	t.Run("top_level_gemini", func(t *testing.T) {
		shared := &LLMConfig{Provider: "gemini", Endpoint: anthropicURL}
		sub := &LLMProviderConfig{}
		inheritLLMDefaults(sub, shared)
		if sub.Endpoint != "" {
			t.Errorf("sub Endpoint: got %q, want empty (so URL builder fires)", sub.Endpoint)
		}
		if sub.Provider != "gemini" {
			t.Errorf("sub Provider: got %q, want gemini", sub.Provider)
		}
	})

	// Case 2: top-level anthropic, sub-block explicitly gemini.
	t.Run("mixed_providers", func(t *testing.T) {
		shared := &LLMConfig{Provider: "anthropic", Endpoint: anthropicURL}
		sub := &LLMProviderConfig{Provider: "gemini"}
		inheritLLMDefaults(sub, shared)
		if sub.Endpoint != "" {
			t.Errorf("sub Endpoint: got %q, want empty", sub.Endpoint)
		}
	})

	// Sanity: anthropic sub-block still inherits the anthropic endpoint.
	t.Run("anthropic_inherits", func(t *testing.T) {
		shared := &LLMConfig{Provider: "anthropic", Endpoint: anthropicURL}
		sub := &LLMProviderConfig{}
		inheritLLMDefaults(sub, shared)
		if sub.Endpoint != anthropicURL {
			t.Errorf("sub Endpoint: got %q, want %q", sub.Endpoint, anthropicURL)
		}
	})

	// Sanity: a custom (non-Anthropic-default) endpoint still inherits — the
	// guard only filters the specific Anthropic default URL.
	t.Run("custom_endpoint_inherits", func(t *testing.T) {
		shared := &LLMConfig{Provider: "gemini", Endpoint: "https://my-gateway.internal/v1"}
		sub := &LLMProviderConfig{}
		inheritLLMDefaults(sub, shared)
		if sub.Endpoint != "https://my-gateway.internal/v1" {
			t.Errorf("sub Endpoint: got %q, want custom URL", sub.Endpoint)
		}
	})
}
