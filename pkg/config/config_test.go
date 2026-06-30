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

func TestLoadAppliesProxyLiteCloudEnv(t *testing.T) {
	t.Setenv("CLAWVISOR_ROUTE_SET", "proxy_lite")
	t.Setenv("CLAWVISOR_PROXY_LITE_ENABLED", "true")
	t.Setenv("CLAWVISOR_PROXY_LITE_PUBLIC_URL", "https://llm.example.com/")
	t.Setenv("CLAWVISOR_PROXY_LITE_ANTHROPIC_BASE_URL", "https://anthropic.internal")
	t.Setenv("CLAWVISOR_PROXY_LITE_OPENAI_BASE_URL", "https://openai.internal")
	t.Setenv("CLAWVISOR_PROXY_LITE_SELF_HOSTNAMES", "app.example.com, llm.example.com")
	t.Setenv("CLAWVISOR_PROXY_LITE_ALLOW_PRIVATE_NETWORKS", "false")
	t.Setenv("CLAWVISOR_PROXY_LITE_TRACE_LOG_PATH", "/tmp/lite-trace.jsonl")
	t.Setenv("CLAWVISOR_PROXY_LITE_RAW_LOG_PATH", "/tmp/lite-raw.jsonl")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.RouteSet != "proxy_lite" {
		t.Fatalf("RouteSet=%q, want proxy_lite", cfg.Server.RouteSet)
	}
	if !cfg.ProxyLite.Enabled {
		t.Fatal("expected proxy lite enabled")
	}
	if cfg.ProxyLite.PublicURL != "https://llm.example.com" {
		t.Fatalf("PublicURL=%q", cfg.ProxyLite.PublicURL)
	}
	if cfg.ProxyLite.AnthropicBaseURL != "https://anthropic.internal" {
		t.Fatalf("AnthropicBaseURL=%q", cfg.ProxyLite.AnthropicBaseURL)
	}
	if cfg.ProxyLite.OpenAIBaseURL != "https://openai.internal" {
		t.Fatalf("OpenAIBaseURL=%q", cfg.ProxyLite.OpenAIBaseURL)
	}
	if got := strings.Join(cfg.ProxyLite.SelfHostnames, ","); got != "app.example.com,llm.example.com" {
		t.Fatalf("SelfHostnames=%q", got)
	}
	if cfg.ProxyLite.AllowPrivateNetworks {
		t.Fatal("expected private networks disabled")
	}
	if cfg.ProxyLite.TraceLogPath != "/tmp/lite-trace.jsonl" {
		t.Fatalf("TraceLogPath=%q", cfg.ProxyLite.TraceLogPath)
	}
	if cfg.ProxyLite.RawLogPath != "/tmp/lite-raw.jsonl" {
		t.Fatalf("RawLogPath=%q", cfg.ProxyLite.RawLogPath)
	}
}

func TestValidateProxyLiteRouteSetRequiresProxyLite(t *testing.T) {
	cfg := Default()
	cfg.Server.RouteSet = "proxy_lite"
	cfg.ProxyLite.Enabled = false

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "proxy_lite.enabled") {
		t.Fatalf("expected proxy_lite.enabled validation error, got %v", err)
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

func TestValidateNotificationEscalationDefaultsDisabled(t *testing.T) {
	cfg := Default()
	if cfg.Notifications.Escalation.Enabled {
		t.Fatal("expected notification escalation disabled by default")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateNotificationEscalationAcceptsPushTelegramChain(t *testing.T) {
	cfg := Default()
	cfg.Notifications.Escalation.Enabled = true
	cfg.Notifications.Escalation.DefaultChain = []NotificationEscalationStep{
		{Channel: "push", DelaySeconds: 0},
		{Channel: "telegram", DelaySeconds: 60},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateNotificationEscalationRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{
			name: "enabled empty chain",
			mut: func(cfg *Config) {
				cfg.Notifications.Escalation.Enabled = true
			},
			want: "default_chain",
		},
		{
			name: "invalid channel",
			mut: func(cfg *Config) {
				cfg.Notifications.Escalation.Enabled = true
				cfg.Notifications.Escalation.DefaultChain = []NotificationEscalationStep{{Channel: "sms"}}
			},
			want: "push, telegram",
		},
		{
			name: "negative delay",
			mut: func(cfg *Config) {
				cfg.Notifications.Escalation.Enabled = true
				cfg.Notifications.Escalation.DefaultChain = []NotificationEscalationStep{{Channel: "push", DelaySeconds: -1}}
			},
			want: "non-negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mut(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected validation error containing %q, got %v", tt.want, err)
			}
		})
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
