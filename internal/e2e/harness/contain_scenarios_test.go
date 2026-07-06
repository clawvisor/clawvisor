package harness

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// The Contain-only deterministic scenarios (spec 09 Tests). They exercise the
// real runtime-proxy egress hook + the Contain superset backstop through the
// in-process harness (which has the Upstreams dial override the full-server
// subprocess lacks), so a direct LLM-host hit can be asserted without network.

// containServer boots the harness with runtime_proxy.llm_route=proxy_lite so
// the Contain superset is active. Server.Config is the same pointer the egress
// hook captured, so mutating it here is observed by the policy layer.
func containServer(t *testing.T) *Server {
	t.Helper()
	srv, err := Start(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("harness.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	srv.Config.RuntimeProxy.LLMRoute = "proxy_lite"
	srv.Config.Server.PublicURL = "https://daemon.clawvisor.test"
	return srv
}

// contain_egress_allow_deny: under Contain, non-LLM egress still flows through
// the runtime proxy and obeys egress policy — an allow rule passes, a deny
// rule 403s. (The Govern pipeline capabilities are unchanged; Contain only
// adds network-layer egress control on top.)
func TestContainEgressAllowDeny(t *testing.T) {
	ctx := context.Background()
	srv := containServer(t)
	srv.Upstreams.AddJSON("api.allowed.test", http.StatusOK, `{"ok":true}`)
	srv.Upstreams.AddJSON("api.denied.test", http.StatusOK, `{"ok":true}`)

	p, err := srv.SeedPrincipal(ctx, "contain-egress")
	if err != nil {
		t.Fatalf("SeedPrincipal: %v", err)
	}
	for _, r := range []*store.RuntimePolicyRule{
		{ID: "allow-host", UserID: p.User.ID, Kind: "egress", Action: "allow", Host: "api.allowed.test", Source: "test", Enabled: true},
		{ID: "deny-host", UserID: p.User.ID, Kind: "egress", Action: "deny", Host: "api.denied.test", Source: "test", Enabled: true},
	} {
		if err := srv.Store.CreateRuntimePolicyRule(ctx, r); err != nil {
			t.Fatalf("CreateRuntimePolicyRule %s: %v", r.ID, err)
		}
	}
	sess, err := srv.CreateSession(ctx, p)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, err := sess.Client.Get("https://api.allowed.test/")
	if err != nil {
		t.Fatalf("allowed GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed host status=%d, want 200", resp.StatusCode)
	}

	resp, err = sess.Client.Get("https://api.denied.test/")
	if err != nil {
		t.Fatalf("denied GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied host status=%d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "RUNTIME_POLICY_DENY") {
		t.Fatalf("denied body should carry RUNTIME_POLICY_DENY, got %s", string(body))
	}
}

// contain_llm_bypass_blocked: a direct request to a known LLM host that dodged
// the env-var routing reaches the runtime proxy and is blocked with 403
// llm_direct_bypass + a runtime.llm_bypass_blocked audit event (enforce mode).
func TestContainLLMBypassBlocked(t *testing.T) {
	ctx := context.Background()
	srv := containServer(t)
	// Register the LLM host as an upstream so that IF the backstop failed to
	// block, the request would succeed (200) and the test would catch it.
	srv.Upstreams.AddJSON("api.anthropic.com", http.StatusOK, `{"leaked":true}`)

	p, err := srv.SeedPrincipal(ctx, "contain-block")
	if err != nil {
		t.Fatalf("SeedPrincipal: %v", err)
	}
	sess, err := srv.CreateSession(ctx, p)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, err := sess.Client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("bypass GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct LLM hit status=%d, want 403 (body=%s)", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "llm_direct_bypass") {
		t.Fatalf("expected llm_direct_bypass error body, got %s", string(body))
	}
	if srv.Upstreams.Hits("api.anthropic.com") != 0 {
		t.Fatalf("backstop must block before upstream; got %d hits", srv.Upstreams.Hits("api.anthropic.com"))
	}

	events, err := srv.Store.ListRuntimeEvents(ctx, p.User.ID, store.RuntimeEventFilter{SessionID: sess.SessionID, Limit: 20})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	ev := findEvent(events, "runtime.llm_bypass_blocked")
	if ev == nil {
		t.Fatalf("expected runtime.llm_bypass_blocked event, got %v", eventTypes(events))
	}
	if ev.Outcome == nil || *ev.Outcome != "blocked" {
		t.Fatalf("expected outcome=blocked, got %v", ev.Outcome)
	}
}

// contain_llm_bypass_warned: same direct LLM hit under observation mode warns
// instead of blocks — the request passes through and a runtime.llm_bypass_blocked
// event with action=warned is recorded (Observe-within-Contain stays
// non-breaking).
func TestContainLLMBypassWarned(t *testing.T) {
	ctx := context.Background()
	srv := containServer(t)
	// Observation mode: the manager reads ObservationModeDefault at create.
	srv.Config.RuntimePolicy.ObservationModeDefault = true
	srv.Upstreams.AddJSON("api.anthropic.com", http.StatusOK, `{"ok":true}`)

	p, err := srv.SeedPrincipal(ctx, "contain-warn")
	if err != nil {
		t.Fatalf("SeedPrincipal: %v", err)
	}
	sess, err := srv.CreateSession(ctx, p)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, err := sess.Client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("warn GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("observe-mode direct LLM hit status=%d, want 200 pass-through (body=%s)", resp.StatusCode, string(body))
	}
	if srv.Upstreams.Hits("api.anthropic.com") == 0 {
		t.Fatal("observe-mode warn should pass the request through to upstream")
	}

	events, err := srv.Store.ListRuntimeEvents(ctx, p.User.ID, store.RuntimeEventFilter{SessionID: sess.SessionID, Limit: 20})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	ev := findEvent(events, "runtime.llm_bypass_blocked")
	if ev == nil {
		t.Fatalf("expected runtime.llm_bypass_blocked event, got %v", eventTypes(events))
	}
	if ev.Outcome == nil || *ev.Outcome != "warned" {
		t.Fatalf("expected outcome=warned, got %v", ev.Outcome)
	}
}

// contain_routing_composition: LLM traffic pointed at the Clawvisor daemon host
// (where ANTHROPIC_BASE_URL/OPENAI_BASE_URL resolve) is allowlisted through the
// runtime proxy (defense-in-depth for NO_PROXY-ignoring clients) and produces
// NO runtime-proxy deny/bypass record — it flows to proxy-lite, not the runtime
// proxy's own handling. A real LLM host, by contrast, is NOT allowlisted.
func TestContainRoutingComposition(t *testing.T) {
	ctx := context.Background()
	srv := containServer(t)
	srv.Upstreams.AddJSON("daemon.clawvisor.test", http.StatusOK, `{"proxy_lite":true}`)

	p, err := srv.SeedPrincipal(ctx, "contain-compose")
	if err != nil {
		t.Fatalf("SeedPrincipal: %v", err)
	}
	sess, err := srv.CreateSession(ctx, p)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, err := sess.Client.Get("https://daemon.clawvisor.test/api/v1/messages")
	if err != nil {
		t.Fatalf("daemon-host GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("daemon-host request status=%d, want 200 (allowlisted bypass)", resp.StatusCode)
	}

	// The allowlisted daemon host short-circuits before any egress audit/deny
	// record is written — proving LLM traffic composes into proxy-lite rather
	// than being processed as runtime-proxy egress.
	events, err := srv.Store.ListRuntimeEvents(ctx, p.User.ID, store.RuntimeEventFilter{SessionID: sess.SessionID, Limit: 20})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	if findEvent(events, "runtime.llm_bypass_blocked") != nil {
		t.Fatal("daemon host must NOT be backstopped")
	}
	if findEvent(events, "runtime.policy.deny_matched") != nil {
		t.Fatal("daemon host must NOT produce a runtime-proxy deny record")
	}
}

// contain_config_validation: the contain preset without proxy_lite.enabled
// fails config load with the llm_route gate error.
func TestContainConfigValidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "posture: contain\nexperimental_contain: true\nproxy_lite:\n  enabled: false\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "llm_route=proxy_lite requires proxy_lite.enabled=true") {
		t.Fatalf("expected contain llm_route gate error, got %v", err)
	}
}

func findEvent(events []*store.RuntimeEvent, eventType string) *store.RuntimeEvent {
	for _, e := range events {
		if e.EventType == eventType {
			return e
		}
	}
	return nil
}
