package proxy

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type sessionRuntimeSettings struct {
	RuntimeEnabled         bool   `json:"runtime_enabled"`
	RuntimeMode            string `json:"runtime_mode"`
	StarterProfile         string `json:"starter_profile"`
	OutboundCredentialMode string `json:"outbound_credential_mode"`
	InjectStoredBearer     bool   `json:"inject_stored_bearer"`

	// Phase 0.6: per-session overrides for fields previously sourced
	// only from cfg.RuntimePolicy. Cloud realizes these from per-org
	// policy at session-create time; daemon mode leaves them zero and
	// the accessors fall back to the global cfg value.
	//
	// HarnessAllowlist uses *[]string so a tenant can explicitly express
	// "empty allowlist" — i.e. the override is `[]`, which means only
	// the built-in defaults (api.anthropic.com etc.) and cfg.LLM.Endpoint
	// pass. A `nil` pointer means "no override; use cfg." Without the
	// pointer, an empty slice is indistinguishable from absent in JSON
	// round-trips, and the allowlist override silently degrades to the
	// global cfg.
	InlineApprovalEnabled   *bool     `json:"inline_approval_enabled,omitempty"`
	ToolLeaseTimeoutSeconds int       `json:"tool_lease_timeout_seconds,omitempty"`
	HarnessAllowlist        *[]string `json:"harness_allowlist,omitempty"`
}

func mergedAgentRuntimeSettings(agent *store.Agent, cfg *config.Config) sessionRuntimeSettings {
	settings := sessionRuntimeSettings{
		RuntimeEnabled:         true,
		RuntimeMode:            "observe",
		StarterProfile:         "none",
		OutboundCredentialMode: "inherit",
		InjectStoredBearer:     cfg != nil && cfg.RuntimePolicy.InjectStoredBearer,
	}
	if cfg != nil && !cfg.RuntimePolicy.ObservationModeDefault {
		settings.RuntimeMode = "enforce"
	}
	if agent != nil && agent.RuntimeSettings != nil {
		settings.RuntimeEnabled = agent.RuntimeSettings.RuntimeEnabled
		settings.RuntimeMode = firstNonEmptyLower(agent.RuntimeSettings.RuntimeMode, settings.RuntimeMode)
		settings.StarterProfile = firstNonEmptyLower(agent.RuntimeSettings.StarterProfile, settings.StarterProfile)
		settings.OutboundCredentialMode = firstNonEmptyLower(agent.RuntimeSettings.OutboundCredentialMode, settings.OutboundCredentialMode)
		settings.InjectStoredBearer = agent.RuntimeSettings.InjectStoredBearer
	}
	return settings
}

func sessionRuntimeSettingsFromMetadata(session *store.RuntimeSession, cfg *config.Config) sessionRuntimeSettings {
	settings := mergedAgentRuntimeSettings(nil, cfg)
	if session == nil || len(session.MetadataJSON) == 0 {
		return settings
	}
	var parsed sessionRuntimeSettings
	if err := json.Unmarshal(session.MetadataJSON, &parsed); err != nil {
		return settings
	}
	if parsed.RuntimeMode != "" {
		settings.RuntimeMode = strings.ToLower(strings.TrimSpace(parsed.RuntimeMode))
	}
	if parsed.StarterProfile != "" {
		settings.StarterProfile = strings.ToLower(strings.TrimSpace(parsed.StarterProfile))
	}
	if parsed.OutboundCredentialMode != "" {
		settings.OutboundCredentialMode = strings.ToLower(strings.TrimSpace(parsed.OutboundCredentialMode))
	}
	settings.RuntimeEnabled = parsed.RuntimeEnabled || settings.RuntimeEnabled
	if hasMetadataBool(session.MetadataJSON, "runtime_enabled") {
		settings.RuntimeEnabled = parsed.RuntimeEnabled
	}
	if hasMetadataBool(session.MetadataJSON, "inject_stored_bearer") {
		settings.InjectStoredBearer = parsed.InjectStoredBearer
	}
	if parsed.InlineApprovalEnabled != nil {
		settings.InlineApprovalEnabled = parsed.InlineApprovalEnabled
	}
	if parsed.ToolLeaseTimeoutSeconds > 0 {
		settings.ToolLeaseTimeoutSeconds = parsed.ToolLeaseTimeoutSeconds
	}
	if parsed.HarnessAllowlist != nil {
		settings.HarnessAllowlist = parsed.HarnessAllowlist
	}
	return settings
}

func sessionAutovaultMode(session *store.RuntimeSession, cfg *config.Config) string {
	settings := sessionRuntimeSettingsFromMetadata(session, cfg)
	switch settings.OutboundCredentialMode {
	case "observe", "strict":
		return settings.OutboundCredentialMode
	case "inherit":
		return autovaultMode(cfg)
	default:
		return autovaultMode(cfg)
	}
}

func sessionShouldInjectStoredBearer(session *store.RuntimeSession, cfg *config.Config) bool {
	settings := sessionRuntimeSettingsFromMetadata(session, cfg)
	return settings.InjectStoredBearer
}

// sessionInlineApprovalEnabled returns whether inline approval is enabled
// for this session. Per-session override (when set) wins; otherwise falls
// back to cfg.RuntimePolicy.InlineApprovalEnabled.
func sessionInlineApprovalEnabled(session *store.RuntimeSession, cfg *config.Config) bool {
	settings := sessionRuntimeSettingsFromMetadata(session, cfg)
	if settings.InlineApprovalEnabled != nil {
		return *settings.InlineApprovalEnabled
	}
	return cfg != nil && cfg.RuntimePolicy.InlineApprovalEnabled
}

// sessionToolLeaseTTL returns the lease TTL for this session. Per-session
// override (positive value) wins; otherwise falls back to cfg.
func sessionToolLeaseTTL(session *store.RuntimeSession, cfg *config.Config) time.Duration {
	settings := sessionRuntimeSettingsFromMetadata(session, cfg)
	if settings.ToolLeaseTimeoutSeconds > 0 {
		return time.Duration(settings.ToolLeaseTimeoutSeconds) * time.Second
	}
	return toolLeaseTTL(cfg)
}

// sessionHarnessAllowlist returns the harness allowlist for this session.
// A non-nil per-session override wins (including an explicit empty list,
// which means "deny everything except the built-in defaults"). A nil
// override falls back to cfg.RuntimePolicy.HarnessAllowlist.
func sessionHarnessAllowlist(session *store.RuntimeSession, cfg *config.Config) []string {
	settings := sessionRuntimeSettingsFromMetadata(session, cfg)
	if settings.HarnessAllowlist != nil {
		return *settings.HarnessAllowlist
	}
	if cfg == nil {
		return nil
	}
	return cfg.RuntimePolicy.HarnessAllowlist
}

func firstNonEmptyLower(values ...string) string {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			return value
		}
	}
	return ""
}

func hasMetadataBool(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false
	}
	_, ok := parsed[key]
	return ok
}
