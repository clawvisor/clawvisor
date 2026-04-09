package adaptergen

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlvalidate"
)

// validate checks a parsed ServiceDef for structural correctness and risk sanity.
// Incomplete actions (e.g. from LLM output truncation) are removed from the def
// and reported as warnings rather than blocking the entire generation.
func validate(def *yamldef.ServiceDef) yamlvalidate.Result {
	// Run the shared validator first (non-mutating).
	result := yamlvalidate.Validate(def)

	// LLM-specific: downgrade oauth2 → api_key when no flow is configured.
	if def.Auth.Type == "oauth2" && def.Auth.OAuth == nil && def.Auth.DeviceFlow == nil && def.Auth.PKCEFlow == nil {
		def.Auth.Type = "api_key"
		result.Warnings = append(result.Warnings, "auth type downgraded from oauth2 to api_key — no OAuth flow was configured (missing pkce_flow/device_flow section with scopes and URLs)")
	}

	// LLM-specific: drop incomplete actions instead of failing the whole def.
	// Collect action-level errors from the shared validator and convert to warnings.
	actionPrefix := "action "
	var keptErrors []string
	droppedActions := map[string]bool{}
	for _, e := range result.Errors {
		if strings.HasPrefix(e, actionPrefix) {
			// Extract action name from 'action "foo": ...' format.
			name := extractActionName(e)
			if name != "" && !droppedActions[name] {
				droppedActions[name] = true
				delete(def.Actions, name)
			}
			result.Warnings = append(result.Warnings, "dropped incomplete "+e)
		} else {
			keptErrors = append(keptErrors, e)
		}
	}
	result.Errors = keptErrors

	// After dropping bad actions, we might have zero left.
	if len(def.Actions) == 0 && len(result.Errors) == 0 {
		result.Errors = append(result.Errors, "no valid actions remain after dropping incomplete ones (output may have been truncated)")
	}

	return result
}

// extractActionName pulls the action name from an error string like `action "foo": some error`.
func extractActionName(e string) string {
	start := strings.Index(e, `"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(e[start+1:], `"`)
	if end < 0 {
		return ""
	}
	return e[start+1 : start+1+end]
}

// parseAndValidate parses raw YAML into a ServiceDef, validates it, drops
// incomplete actions (with warnings), and returns errors only for fatal issues.
func parseAndValidate(yamlBytes []byte) (yamldef.ServiceDef, []string, []string, error) {
	var def yamldef.ServiceDef
	if err := yaml.Unmarshal(yamlBytes, &def); err != nil {
		return def, nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}
	result := validate(&def)
	return def, result.Errors, result.Warnings, nil
}

// hasUnclassifiedRisk checks if any action still has UNCLASSIFIED risk placeholders.
func hasUnclassifiedRisk(yamlContent string) bool {
	return strings.Contains(yamlContent, "UNCLASSIFIED")
}
