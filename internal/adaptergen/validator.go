package adaptergen

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// safeServiceIDPattern matches only valid service IDs: lowercase alphanumeric
// segments separated by dots (e.g. "jira", "google.gmail", "pagerduty").
// No path separators, no "..", no whitespace.
var safeServiceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)

// validRiskCategories is the set of allowed risk categories.
var validRiskCategories = map[string]bool{
	"read": true, "write": true, "delete": true, "search": true,
}

// validSensitivities is the set of allowed sensitivity levels.
var validSensitivities = map[string]bool{
	"low": true, "medium": true, "high": true,
}

// validAuthTypes is the set of allowed authentication types.
var validAuthTypes = map[string]bool{
	"api_key": true, "oauth2": true, "basic": true, "none": true,
}

// validHTTPMethods is the set of allowed HTTP methods.
var validHTTPMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// validateResult holds both hard errors (service-level) and soft warnings (dropped actions).
type validateResult struct {
	Errors   []string // fatal — prevent installation
	Warnings []string // informational — incomplete actions that were dropped
}

// validate checks a parsed ServiceDef for structural correctness and risk sanity.
// Incomplete actions (e.g. from LLM output truncation) are removed from the def
// and reported as warnings rather than blocking the entire generation.
func validate(def *yamldef.ServiceDef) validateResult {
	var result validateResult

	// Service metadata.
	if def.Service.ID == "" {
		result.Errors = append(result.Errors, "service.id is required")
	} else if !safeServiceIDPattern.MatchString(def.Service.ID) {
		result.Errors = append(result.Errors, fmt.Sprintf("service.id %q contains invalid characters (must be lowercase alphanumeric segments separated by dots)", def.Service.ID))
	}
	if def.Service.DisplayName == "" {
		result.Errors = append(result.Errors, "service.display_name is required")
	}

	// Auth type.
	if def.Auth.Type != "" && !validAuthTypes[def.Auth.Type] {
		result.Errors = append(result.Errors, fmt.Sprintf("invalid auth type: %q", def.Auth.Type))
	}

	// If the LLM set oauth2 but didn't include any flow config, the user will
	// be prompted for an API key with no way to do OAuth. Warn about this.
	if def.Auth.Type == "oauth2" && def.Auth.OAuth == nil && def.Auth.DeviceFlow == nil && def.Auth.PKCEFlow == nil {
		def.Auth.Type = "api_key"
		result.Warnings = append(result.Warnings, "auth type downgraded from oauth2 to api_key — no OAuth flow was configured (missing pkce_flow/device_flow section with scopes and URLs)")
	}

	// API.
	if def.API.BaseURL == "" {
		result.Errors = append(result.Errors, "api.base_url is required")
	}

	// Validate each action — drop incomplete ones instead of failing.
	if len(def.Actions) == 0 {
		result.Errors = append(result.Errors, "at least one action is required")
	}

	for name, action := range def.Actions {
		actionErrs := validateAction(name, action, def.API.Type)
		if len(actionErrs) > 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("dropped incomplete action %q: %s", name, strings.Join(actionErrs, "; ")))
			delete(def.Actions, name)
		}
	}

	// After dropping bad actions, we might have zero left.
	if len(def.Actions) == 0 && len(result.Errors) == 0 {
		result.Errors = append(result.Errors, "no valid actions remain after dropping incomplete ones (output may have been truncated)")
	}

	return result
}

// validateAction checks a single action for completeness and returns any errors.
func validateAction(name string, action yamldef.Action, apiType string) []string {
	var errs []string

	if action.DisplayName == "" {
		errs = append(errs, "display_name is required")
	}

	// Risk validation.
	if !validRiskCategories[action.Risk.Category] {
		errs = append(errs, fmt.Sprintf("risk.category %q is invalid", action.Risk.Category))
	}
	if !validSensitivities[action.Risk.Sensitivity] {
		errs = append(errs, fmt.Sprintf("risk.sensitivity %q is invalid", action.Risk.Sensitivity))
	}

	// Risk sanity checks.
	if action.Method == "DELETE" && action.Risk.Category != "delete" {
		errs = append(errs, fmt.Sprintf("DELETE method should have risk category 'delete', got %q", action.Risk.Category))
	}
	if action.Method == "DELETE" && action.Risk.Sensitivity == "low" {
		errs = append(errs, "DELETE method cannot have 'low' sensitivity")
	}
	if (action.Method == "POST" || action.Method == "PUT" || action.Method == "PATCH") &&
		action.Risk.Category == "read" {
		errs = append(errs, fmt.Sprintf("%s method should not have risk category 'read'", action.Method))
	}

	// Method validation (REST only).
	if apiType != "graphql" {
		if action.Method == "" {
			errs = append(errs, "method is required for REST actions")
		} else if !validHTTPMethods[action.Method] {
			errs = append(errs, fmt.Sprintf("method %q is invalid", action.Method))
		}
		if action.Path == "" {
			errs = append(errs, "path is required for REST actions")
		}
	}

	return errs
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
