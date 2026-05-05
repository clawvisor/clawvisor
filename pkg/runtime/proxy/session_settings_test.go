package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/review"
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

	// Populated override wins
	raw, _ := json.Marshal(map[string]any{"harness_allowlist": []string{"override.example"}})
	sess := &store.RuntimeSession{MetadataJSON: raw}
	got := sessionHarnessAllowlist(sess, cfg)
	if len(got) != 1 || got[0] != "override.example" {
		t.Errorf("populated override: got %v", got)
	}

	// Explicit empty override wins — must NOT silently fall back to cfg.
	// This is the case a tenant uses to express "deny everything except
	// the built-in defaults and cfg.LLM.Endpoint."
	rawEmpty, _ := json.Marshal(map[string]any{"harness_allowlist": []string{}})
	sessEmpty := &store.RuntimeSession{MetadataJSON: rawEmpty}
	gotEmpty := sessionHarnessAllowlist(sessEmpty, cfg)
	if gotEmpty == nil {
		t.Errorf("explicit empty override should yield non-nil empty slice, got nil")
	}
	if len(gotEmpty) != 0 {
		t.Errorf("explicit empty override should yield empty slice, got %v (silently fell back to cfg?)", gotEmpty)
	}
}

// TestApprovalSurface_RespectsSessionOverride confirms that the
// held-approval Surface field and the rendered prompt text both honor
// the per-session InlineApproval override, not just the global cfg.
// Without this, a session-level disable of inline approvals (or a
// session-level enable when cfg has it off) produces an inconsistent
// approval experience: cfg says "dashboard" but session says "inline",
// or vice versa.
func TestApprovalSurface_RespectsSessionOverride(t *testing.T) {
	cfgInline := &config.Config{}
	cfgInline.RuntimePolicy.InlineApprovalEnabled = true
	cfgDashboard := &config.Config{}
	cfgDashboard.RuntimePolicy.InlineApprovalEnabled = false

	inlineOn := true
	inlineOff := false
	rawInlineOn, _ := json.Marshal(map[string]any{"inline_approval_enabled": true})
	rawInlineOff, _ := json.Marshal(map[string]any{"inline_approval_enabled": false})
	sessInlineOn := &store.RuntimeSession{MetadataJSON: rawInlineOn}
	sessInlineOff := &store.RuntimeSession{MetadataJSON: rawInlineOff}
	_ = inlineOn
	_ = inlineOff

	// cfg=dashboard, session=inline → inline wins
	if got := approvalSurface(sessInlineOn, cfgDashboard); got != "inline" {
		t.Errorf("session=inline, cfg=dashboard → approvalSurface=%q, want inline", got)
	}
	// cfg=inline, session=dashboard → dashboard wins
	if got := approvalSurface(sessInlineOff, cfgInline); got != "dashboard" {
		t.Errorf("session=dashboard, cfg=inline → approvalSurface=%q, want dashboard", got)
	}

	// Held-approval prompt rendering follows the same rule.
	held := &review.HeldApproval{ID: "abc", ToolName: "Bash", ToolInput: map[string]any{"cmd": "ls"}}
	if got := renderHeldToolUsePrompt(held, sessInlineOn, cfgDashboard); !strings.Contains(got, "Reply `approve`") {
		t.Errorf("session=inline override should produce inline prompt; got %q", got)
	}
	if got := renderHeldToolUsePrompt(held, sessInlineOff, cfgInline); !strings.Contains(got, "Pending approval in the dashboard") {
		t.Errorf("session=dashboard override should produce dashboard prompt; got %q", got)
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

	// Explicit empty allowlist: built-in defaults still pass, but cfg
	// fallback must NOT kick in (the tenant deliberately wanted no
	// extra allowed hosts beyond the built-ins).
	cfgWithGlobal := &config.Config{}
	cfgWithGlobal.RuntimePolicy.HarnessAllowlist = []string{"global.example"}
	rawEmpty, _ := json.Marshal(map[string]any{"harness_allowlist": []string{}})
	sessEmpty := &store.RuntimeSession{MetadataJSON: rawEmpty}
	if !isHarnessAllowlistedForSession(sessEmpty, cfgWithGlobal, "api.anthropic.com") {
		t.Errorf("built-in defaults must still pass under explicit empty override")
	}
	if isHarnessAllowlistedForSession(sessEmpty, cfgWithGlobal, "global.example") {
		t.Errorf("explicit empty override must NOT silently fall back to cfg allowlist")
	}
}
