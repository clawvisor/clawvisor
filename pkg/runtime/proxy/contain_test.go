package proxy

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func containCfg() *config.Config {
	cfg := &config.Config{}
	cfg.RuntimeProxy.LLMRoute = "proxy_lite"
	cfg.Server.PublicURL = "https://clawvisor.example.com"
	return cfg
}

// TestContainAllowlistSkipsHardcodedLLMHosts asserts spec 09 D2: in the
// Contain superset (llm_route=proxy_lite) the hardcoded provider LLM hosts are
// NOT allowlisted — they must fall through so the backstop/egress policy can
// deny them. Legacy (direct) route keeps allowing them.
func TestContainAllowlistSkipsHardcodedLLMHosts(t *testing.T) {
	contain := containCfg()
	for _, host := range []string{"api.anthropic.com", "api.openai.com", "chatgpt.com"} {
		if isHarnessAllowlistedForSession(nil, contain, host) {
			t.Errorf("contain: %s must NOT be allowlisted (backstop territory)", host)
		}
	}

	legacy := &config.Config{}
	for _, host := range []string{"api.anthropic.com", "api.openai.com", "chatgpt.com"} {
		if !isHarnessAllowlistedForSession(nil, legacy, host) {
			t.Errorf("legacy: %s should stay allowlisted", host)
		}
	}
}

// TestContainAllowsDaemonHost asserts spec 09 D1.3: the Clawvisor daemon host
// (Server.PublicURL) is allowlisted so a client that ignores NO_PROXY still
// reaches proxy-lite through the runtime proxy.
func TestContainAllowsDaemonHost(t *testing.T) {
	contain := containCfg()
	if !isHarnessAllowlistedForSession(nil, contain, "clawvisor.example.com") {
		t.Error("contain: the daemon host must be allowlisted for NO_PROXY-ignoring clients")
	}
}

// TestContainBackstopHostSet asserts the backstop host set covers all three
// provider hosts including chatgpt.com (gotcha 4).
func TestContainBackstopHostSet(t *testing.T) {
	for _, host := range []string{"api.anthropic.com", "api.openai.com", "chatgpt.com"} {
		if !isContainLLMBackstopHost(host) {
			t.Errorf("%s should be a backstop host", host)
		}
	}
	if isContainLLMBackstopHost("example.com") {
		t.Error("non-LLM host must not be a backstop host")
	}
}

// TestSessionLLMRouteProxyLite covers cfg fallback and per-session override.
func TestSessionLLMRouteProxyLite(t *testing.T) {
	if !sessionLLMRouteProxyLite(nil, containCfg()) {
		t.Error("cfg llm_route=proxy_lite should report proxy_lite")
	}
	if sessionLLMRouteProxyLite(nil, &config.Config{}) {
		t.Error("empty cfg llm_route should report direct")
	}
	// Per-session override (cloud path): metadata sets llm_route explicitly.
	sess := &store.RuntimeSession{MetadataJSON: []byte(`{"llm_route":"proxy_lite"}`)}
	if !sessionLLMRouteProxyLite(sess, &config.Config{}) {
		t.Error("per-session llm_route override should win over empty cfg")
	}
}
