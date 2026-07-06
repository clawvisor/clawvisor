package config

import (
	"os"
	"path/filepath"
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

func TestObservabilityOTelDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Observability.OTel.Enabled {
		t.Fatal("expected observability.otel.enabled to default false")
	}
	if cfg.Observability.OTel.Protocol != "grpc" {
		t.Fatalf("Protocol=%q, want grpc", cfg.Observability.OTel.Protocol)
	}
	if cfg.Observability.OTel.ServiceName != "clawvisor" {
		t.Fatalf("ServiceName=%q, want clawvisor", cfg.Observability.OTel.ServiceName)
	}
	if cfg.Observability.OTel.TraceSampleRatio != 1.0 {
		t.Fatalf("TraceSampleRatio=%v, want 1.0", cfg.Observability.OTel.TraceSampleRatio)
	}
	if cfg.Observability.OTel.MetricsIntervalSec != 60 {
		t.Fatalf("MetricsIntervalSec=%d, want 60", cfg.Observability.OTel.MetricsIntervalSec)
	}
}

func TestLoadAppliesObservabilityOTelEnv(t *testing.T) {
	t.Setenv("CLAWVISOR_OTEL_ENABLED", "true")
	t.Setenv("CLAWVISOR_OTEL_ENDPOINT", "otel-collector:4317")
	t.Setenv("CLAWVISOR_OTEL_PROTOCOL", "http")
	t.Setenv("CLAWVISOR_OTEL_INSECURE", "true")
	t.Setenv("CLAWVISOR_OTEL_SERVICE_NAME", "clawvisor-edge")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Observability.OTel.Enabled {
		t.Fatal("expected otel enabled via env")
	}
	if cfg.Observability.OTel.Endpoint != "otel-collector:4317" {
		t.Fatalf("Endpoint=%q", cfg.Observability.OTel.Endpoint)
	}
	if cfg.Observability.OTel.Protocol != "http" {
		t.Fatalf("Protocol=%q", cfg.Observability.OTel.Protocol)
	}
	if !cfg.Observability.OTel.Insecure {
		t.Fatal("expected insecure enabled via env")
	}
	if cfg.Observability.OTel.ServiceName != "clawvisor-edge" {
		t.Fatalf("ServiceName=%q", cfg.Observability.OTel.ServiceName)
	}
}

func TestValidateObservabilityOTelRules(t *testing.T) {
	t.Run("enabled requires endpoint", func(t *testing.T) {
		cfg := Default()
		cfg.Observability.OTel.Enabled = true
		cfg.Observability.OTel.Endpoint = "  "
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "observability.otel.endpoint") {
			t.Fatalf("expected endpoint validation error, got %v", err)
		}
	})
	t.Run("protocol must be grpc or http", func(t *testing.T) {
		cfg := Default()
		cfg.Observability.OTel.Protocol = "thrift"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "observability.otel.protocol") {
			t.Fatalf("expected protocol validation error, got %v", err)
		}
	})
	t.Run("sample ratio in range", func(t *testing.T) {
		cfg := Default()
		cfg.Observability.OTel.TraceSampleRatio = 1.5
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "observability.otel.trace_sample_ratio") {
			t.Fatalf("expected sample ratio validation error, got %v", err)
		}
	})
}

// --- spec 02: posture presets, config schema marker, knob precedence ---

func writePostureConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestFlipDefaultStaysFalse locks the writer-side flip invariant (PRD §11,
// spec 08): the compiled default for proxy_lite.enabled is false and must
// stay false permanently, so an existing install lacking the key is never
// silently enabled at a binary upgrade. Enforcement/upstream defaults carry
// today's behavior (enforce + vault) but the enablement bit does not move.
func TestFlipDefaultStaysFalse(t *testing.T) {
	d := Default()
	if d.ProxyLite.Enabled {
		t.Fatal("Default().ProxyLite.Enabled must stay false permanently (writer-side flip)")
	}
	if d.ProxyLite.EnforcementMode != "enforce" {
		t.Fatalf("Default enforcement_mode=%q, want enforce", d.ProxyLite.EnforcementMode)
	}
	if d.ProxyLite.UpstreamAuth != "vault" {
		t.Fatalf("Default upstream_auth=%q, want vault", d.ProxyLite.UpstreamAuth)
	}
	if d.ProxyLite.AllowSubscriptionBillingMigration {
		t.Fatal("Default allow_subscription_billing_migration must be false")
	}
}

// TestProxyLiteEnableMatrix asserts exactly which (key present/absent) ×
// (config_schema) × (posture) × (env override) combinations enable
// proxy-lite. Absent key with no posture/env is always disabled regardless
// of the advisory marker — the marker never drives behavior.
func TestProxyLiteEnableMatrix(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		envEnabled  string // "" = unset
		wantEnabled bool
	}{
		{"absent key, schema 0, no posture", "config_schema: 0\n", "", false},
		{"absent key, schema 2, no posture", "config_schema: 2\n", "", false},
		{"explicit false, schema 2", "config_schema: 2\nproxy_lite:\n  enabled: false\n", "", false},
		{"explicit true, schema 2", "config_schema: 2\nproxy_lite:\n  enabled: true\n", "", true},
		{"explicit true, schema 0", "config_schema: 0\nproxy_lite:\n  enabled: true\n", "", true},
		{"posture observe, absent key", "posture: observe\n", "", true},
		{"posture observe, explicit false beats preset", "posture: observe\nproxy_lite:\n  enabled: false\n", "", false},
		{"posture govern, absent key", "posture: govern\n", "", true},
		{"no posture, env true", "config_schema: 2\n", "true", true},
		{"posture observe, env false beats preset", "posture: observe\n", "false", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envEnabled != "" {
				t.Setenv("CLAWVISOR_PROXY_LITE_ENABLED", tc.envEnabled)
			}
			cfg, err := Load(writePostureConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ProxyLite.Enabled != tc.wantEnabled {
				t.Fatalf("ProxyLite.Enabled=%v, want %v", cfg.ProxyLite.Enabled, tc.wantEnabled)
			}
		})
	}
}

// TestPosturePresetKnobPrecedence covers the knob-beats-preset matrix for
// enforcement_mode and upstream_auth: a preset sets a knob only when the YAML
// did not set it explicitly, and an explicit env override always wins.
func TestPosturePresetKnobPrecedence(t *testing.T) {
	t.Run("observe preset sets both knobs", func(t *testing.T) {
		cfg, err := Load(writePostureConfig(t, "posture: observe\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.ProxyLite.Enabled || !cfg.ProxyLite.ObserveMode() || !cfg.ProxyLite.PassthroughUpstreamAuth() {
			t.Fatalf("observe preset: enabled=%v observe=%v passthrough=%v",
				cfg.ProxyLite.Enabled, cfg.ProxyLite.ObserveMode(), cfg.ProxyLite.PassthroughUpstreamAuth())
		}
	})
	t.Run("govern preset sets enforce+vault", func(t *testing.T) {
		cfg, err := Load(writePostureConfig(t, "posture: govern\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.ProxyLite.Enabled || cfg.ProxyLite.ObserveMode() || cfg.ProxyLite.PassthroughUpstreamAuth() {
			t.Fatalf("govern preset: enabled=%v observe=%v passthrough=%v",
				cfg.ProxyLite.Enabled, cfg.ProxyLite.ObserveMode(), cfg.ProxyLite.PassthroughUpstreamAuth())
		}
	})
	t.Run("explicit enforcement_mode beats observe preset", func(t *testing.T) {
		cfg, err := Load(writePostureConfig(t, "posture: observe\nproxy_lite:\n  enforcement_mode: enforce\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ProxyLite.ObserveMode() {
			t.Fatal("explicit enforcement_mode: enforce should override observe preset")
		}
		// upstream_auth was NOT set explicitly, so the preset still applies.
		if !cfg.ProxyLite.PassthroughUpstreamAuth() {
			t.Fatal("upstream_auth should still be passthrough from the observe preset")
		}
	})
	t.Run("env enforcement_mode beats preset", func(t *testing.T) {
		t.Setenv("CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE", "enforce")
		cfg, err := Load(writePostureConfig(t, "posture: observe\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ProxyLite.ObserveMode() {
			t.Fatal("env CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE=enforce should beat observe preset")
		}
	})
}

// TestValidatePostureContainRejected asserts the spec 09 gate semantics that
// replaced spec 02's unconditional rejection: contain is refused UNLESS
// experimental_contain=true, and accepted (with the superset preset applied)
// once the flag is present.
func TestValidatePostureContainRejected(t *testing.T) {
	t.Run("rejected without experimental_contain", func(t *testing.T) {
		cfg := Default()
		cfg.Posture = "contain"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "posture contain requires experimental_contain=true") {
			t.Fatalf("expected contain gate rejection, got %v", err)
		}
	})
	t.Run("accepted with experimental_contain and superset preset", func(t *testing.T) {
		cfg := Default()
		cfg.Posture = "contain"
		cfg.ExperimentalContain = true
		// Mimic the applied contain preset (Validate runs post-preset).
		cfg.ProxyLite.Enabled = true
		cfg.RuntimeProxy.Enabled = true
		cfg.RuntimeProxy.LLMRoute = "proxy_lite"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected contain to validate with the gate flag, got %v", err)
		}
	})
	t.Run("contain preset without proxy_lite.enabled fails llm_route gate", func(t *testing.T) {
		cfg := Default()
		cfg.Posture = "contain"
		cfg.ExperimentalContain = true
		cfg.ProxyLite.Enabled = false
		cfg.RuntimeProxy.Enabled = true
		cfg.RuntimeProxy.LLMRoute = "proxy_lite"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "llm_route=proxy_lite requires proxy_lite.enabled=true") {
			t.Fatalf("expected llm_route gate error, got %v", err)
		}
	})
}

// TestContainPresetAppliesSuperset asserts that loading a contain-postured
// config (with the experimental gate) applies the Govern preset PLUS the
// runtime-proxy superset knobs (enabled + llm_route=proxy_lite), per spec 09.
func TestContainPresetAppliesSuperset(t *testing.T) {
	cfg, err := Load(writePostureConfig(t, "posture: contain\nexperimental_contain: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProxyLite.Enabled || cfg.ProxyLite.ObserveMode() || cfg.ProxyLite.PassthroughUpstreamAuth() {
		t.Fatalf("contain preset should apply govern proxy_lite knobs: enabled=%v observe=%v passthrough=%v",
			cfg.ProxyLite.Enabled, cfg.ProxyLite.ObserveMode(), cfg.ProxyLite.PassthroughUpstreamAuth())
	}
	if !cfg.RuntimeProxy.Enabled {
		t.Fatal("contain preset should enable the runtime proxy")
	}
	if !cfg.RuntimeProxy.LLMRouteProxyLite() {
		t.Fatalf("contain preset should set llm_route=proxy_lite, got %q", cfg.RuntimeProxy.LLMRoute)
	}
}

// TestContainPresetWithoutGateFailsLoad asserts that contain without the
// experimental gate fails startup validation (Load applies the preset; the
// server rejects it at Validate()).
func TestContainPresetWithoutGateFailsLoad(t *testing.T) {
	cfg, err := Load(writePostureConfig(t, "posture: contain\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "posture contain requires experimental_contain=true") {
		t.Fatalf("expected contain Validate to fail on the gate, got %v", err)
	}
}

// TestDefaultLLMRouteIsDirect locks the legacy-safe default: llm_route is
// "direct" so an install that never opts into contain keeps today's behavior.
func TestDefaultLLMRouteIsDirect(t *testing.T) {
	if d := Default(); d.RuntimeProxy.LLMRoute != "direct" {
		t.Fatalf("Default().RuntimeProxy.LLMRoute=%q, want direct", d.RuntimeProxy.LLMRoute)
	}
}

func TestValidatePostureUnknownRejected(t *testing.T) {
	cfg := Default()
	cfg.Posture = "lockdown"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "posture must be one of") {
		t.Fatalf("expected unknown-posture rejection, got %v", err)
	}
}

func TestValidateEnforcementModeEnum(t *testing.T) {
	cfg := Default()
	cfg.ProxyLite.EnforcementMode = "audit"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "proxy_lite.enforcement_mode") {
		t.Fatalf("expected enforcement_mode enum error, got %v", err)
	}
}

func TestValidateUpstreamAuthEnum(t *testing.T) {
	cfg := Default()
	cfg.ProxyLite.UpstreamAuth = "byo"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "proxy_lite.upstream_auth") {
		t.Fatalf("expected upstream_auth enum error, got %v", err)
	}
}

func TestLoadAppliesSubMigrationEnv(t *testing.T) {
	t.Setenv("CLAWVISOR_PROXY_LITE_ALLOW_SUB_MIGRATION", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProxyLite.AllowSubscriptionBillingMigration {
		t.Fatal("expected allow_subscription_billing_migration env override to apply")
	}
}
