package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestSessionInlineApprovalEnabled_Override(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimePolicy.InlineApprovalEnabled = false

	tt := []struct {
		name     string
		metadata map[string]any
		want     bool
	}{
		{name: "no override → falls through to cfg false", metadata: map[string]any{}, want: false},
		{name: "explicit true override wins over cfg false", metadata: map[string]any{"inline_approval_enabled": true}, want: true},
		{name: "explicit false override wins over cfg true (after toggle below)", metadata: map[string]any{"inline_approval_enabled": false}, want: false},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.metadata)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			sess := &store.RuntimeSession{MetadataJSON: raw, ExpiresAt: time.Now().Add(time.Hour)}
			got := sessionInlineApprovalEnabled(sess, cfg)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// Confirm cfg fallback when metadata says nothing.
	cfg.RuntimePolicy.InlineApprovalEnabled = true
	if !sessionInlineApprovalEnabled(&store.RuntimeSession{}, cfg) {
		t.Errorf("expected cfg fallback to surface true")
	}
}

func TestSessionToolLeaseTTL_Override(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimePolicy.ToolLeaseTimeoutSeconds = 60

	// No override → cfg value
	if got := sessionToolLeaseTTL(&store.RuntimeSession{}, cfg); got != 60*time.Second {
		t.Errorf("no-override fallback: got %v, want 60s", got)
	}

	// Override wins
	raw, _ := json.Marshal(map[string]any{"tool_lease_timeout_seconds": 300})
	sess := &store.RuntimeSession{MetadataJSON: raw}
	if got := sessionToolLeaseTTL(sess, cfg); got != 300*time.Second {
		t.Errorf("override: got %v, want 300s", got)
	}
}

func TestSessionHarnessAllowlist_Override(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimePolicy.HarnessAllowlist = []string{"global.example"}

	// No override → cfg value
	if got := sessionHarnessAllowlist(&store.RuntimeSession{}, cfg); len(got) != 1 || got[0] != "global.example" {
		t.Errorf("no-override fallback: got %v", got)
	}

	// Override wins
	raw, _ := json.Marshal(map[string]any{"harness_allowlist": []string{"override.example"}})
	sess := &store.RuntimeSession{MetadataJSON: raw}
	got := sessionHarnessAllowlist(sess, cfg)
	if len(got) != 1 || got[0] != "override.example" {
		t.Errorf("override: got %v", got)
	}
}

func TestIsHarnessAllowlistedForSession_OverrideAndDefaults(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimePolicy.HarnessAllowlist = nil

	// Built-in defaults always pass
	if !isHarnessAllowlistedForSession(nil, cfg, "api.anthropic.com") {
		t.Errorf("api.anthropic.com should always be allowed")
	}

	// Per-session override
	raw, _ := json.Marshal(map[string]any{"harness_allowlist": []string{"my.org"}})
	sess := &store.RuntimeSession{MetadataJSON: raw}
	if !isHarnessAllowlistedForSession(sess, cfg, "my.org") {
		t.Errorf("per-session override should allow my.org")
	}
	if !isHarnessAllowlistedForSession(sess, cfg, "sub.my.org") {
		t.Errorf("per-session override should allow sub.my.org as suffix match")
	}
	if isHarnessAllowlistedForSession(sess, cfg, "elsewhere.example") {
		t.Errorf("per-session override should not allow elsewhere.example")
	}
}
