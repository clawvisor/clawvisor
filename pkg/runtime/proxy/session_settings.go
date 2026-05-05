package proxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type sessionRuntimeSettings struct {
	RuntimeEnabled         bool   `json:"runtime_enabled"`
	RuntimeMode            string `json:"runtime_mode"`
	StarterProfile         string `json:"starter_profile"`
	OutboundCredentialMode string `json:"outbound_credential_mode"`
	InjectStoredBearer     bool   `json:"inject_stored_bearer"`
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
