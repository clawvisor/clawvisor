// Package yamlvalidate checks YAML adapter definitions for structural correctness,
// variable consistency, path param references, and risk sanity.
package yamlvalidate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// SafeServiceIDPattern matches only valid service IDs: lowercase alphanumeric
// segments separated by dots (e.g. "jira", "google.gmail", "pagerduty").
var SafeServiceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)

// varPlaceholderRe matches {{.var.NAME}} placeholders.
var varPlaceholderRe = regexp.MustCompile(`\{\{\.var\.(\w+)\}\}`)

// pathParamRe matches {{.NAME}} placeholders in action paths (but not {{.var.NAME}}).
var pathParamRe = regexp.MustCompile(`\{\{\.(\w+)\}\}`)

// ValidRiskCategories is the set of allowed risk categories.
var ValidRiskCategories = map[string]bool{
	"read": true, "write": true, "delete": true, "search": true,
}

// ValidSensitivities is the set of allowed sensitivity levels.
var ValidSensitivities = map[string]bool{
	"low": true, "medium": true, "high": true,
}

// ValidAuthTypes is the set of allowed authentication types.
var ValidAuthTypes = map[string]bool{
	"api_key": true, "oauth2": true, "basic": true, "none": true,
}

// ValidHTTPMethods is the set of allowed HTTP methods.
var ValidHTTPMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// validParamTypes is the set of allowed parameter types.
var validParamTypes = map[string]bool{
	"string": true, "int": true, "bool": true, "object": true, "array": true,
}

// validParamLocations is the set of allowed parameter locations.
var validParamLocations = map[string]bool{
	"query": true, "body": true, "path": true,
}

// validAPITypes is the set of allowed API types.
var validAPITypes = map[string]bool{
	"rest": true, "graphql": true,
}

// Result holds both hard errors and soft warnings from validation.
type Result struct {
	Errors   []string // fatal — prevent loading/installation
	Warnings []string // informational — adapter still loads
}

// OK returns true if there are no errors.
func (r Result) OK() bool { return len(r.Errors) == 0 }

// Validate checks a parsed ServiceDef for structural correctness and returns
// errors and warnings. It does NOT mutate the def.
func Validate(def *yamldef.ServiceDef) Result {
	var r Result

	// ── Service metadata ────────────────────────────────────────────────
	if def.Service.ID == "" {
		r.Errors = append(r.Errors, "service.id is required")
	} else if !SafeServiceIDPattern.MatchString(def.Service.ID) {
		r.Errors = append(r.Errors, fmt.Sprintf("service.id %q contains invalid characters (must be lowercase alphanumeric segments separated by dots)", def.Service.ID))
	}
	if def.Service.DisplayName == "" {
		r.Errors = append(r.Errors, "service.display_name is required")
	}

	// ── Auth ────────────────────────────────────────────────────────────
	if def.Auth.Type != "" && !ValidAuthTypes[def.Auth.Type] {
		r.Errors = append(r.Errors, fmt.Sprintf("invalid auth.type: %q", def.Auth.Type))
	}
	if def.Auth.Type == "oauth2" && def.Auth.OAuth == nil && def.Auth.DeviceFlow == nil && def.Auth.PKCEFlow == nil {
		r.Warnings = append(r.Warnings, "auth.type is oauth2 but no OAuth flow is configured (missing pkce_flow/device_flow/oauth section)")
	}

	// ── API ─────────────────────────────────────────────────────────────
	if def.API.BaseURL == "" {
		r.Errors = append(r.Errors, "api.base_url is required")
	}
	if def.API.Type != "" && !validAPITypes[def.API.Type] {
		r.Errors = append(r.Errors, fmt.Sprintf("invalid api.type: %q (must be \"rest\" or \"graphql\")", def.API.Type))
	}

	// ── Variables ───────────────────────────────────────────────────────
	referencedVars := extractVarPlaceholders(def.API.BaseURL)
	definedVars := map[string]bool{}
	for name := range def.Variables {
		definedVars[name] = true
	}

	// Every {{.var.X}} in base_url must be defined.
	for _, name := range referencedVars {
		if !definedVars[name] {
			r.Errors = append(r.Errors, fmt.Sprintf("base_url references {{.var.%s}} but no variable %q is defined", name, name))
		}
	}

	// Orphan variables (defined but not referenced in base_url).
	referencedSet := map[string]bool{}
	for _, name := range referencedVars {
		referencedSet[name] = true
	}
	for name := range def.Variables {
		if !referencedSet[name] {
			r.Warnings = append(r.Warnings, fmt.Sprintf("variable %q is defined but not referenced in base_url", name))
		}
	}

	// Required variables should have a display_name.
	for name, v := range def.Variables {
		if v.Required && v.DisplayName == "" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("variable %q is required but has no display_name", name))
		}
	}

	// ── Actions ─────────────────────────────────────────────────────────
	if len(def.Actions) == 0 {
		r.Errors = append(r.Errors, "at least one action is required")
	}

	for name, action := range def.Actions {
		for _, e := range validateAction(name, action, def.API.Type) {
			r.Errors = append(r.Errors, fmt.Sprintf("action %q: %s", name, e))
		}
		for _, w := range validateActionWarnings(name, action) {
			r.Warnings = append(r.Warnings, fmt.Sprintf("action %q: %s", name, w))
		}
	}

	return r
}

// validateAction checks a single action for errors.
func validateAction(_ string, action yamldef.Action, apiType string) []string {
	var errs []string

	if action.DisplayName == "" {
		errs = append(errs, "display_name is required")
	}

	// Risk validation.
	if !ValidRiskCategories[action.Risk.Category] {
		errs = append(errs, fmt.Sprintf("risk.category %q is invalid", action.Risk.Category))
	}
	if !ValidSensitivities[action.Risk.Sensitivity] {
		errs = append(errs, fmt.Sprintf("risk.sensitivity %q is invalid", action.Risk.Sensitivity))
	}

	// Risk sanity checks.
	if action.Method == "DELETE" && action.Risk.Category != "delete" {
		errs = append(errs, fmt.Sprintf("DELETE method should have risk category \"delete\", got %q", action.Risk.Category))
	}
	if action.Method == "DELETE" && action.Risk.Sensitivity == "low" {
		errs = append(errs, "DELETE method cannot have \"low\" sensitivity")
	}
	// PUT/PATCH with "read" is almost certainly wrong; POST with "read" is common
	// for search/query endpoints (e.g. Notion), so we only warn for POST.
	if (action.Method == "PUT" || action.Method == "PATCH") && action.Risk.Category == "read" {
		errs = append(errs, fmt.Sprintf("%s method should not have risk category \"read\"", action.Method))
	}

	// Go-override actions delegate to compiled code — skip method/path/query checks.
	if action.Override != "go" {
		// REST validation.
		if apiType != "graphql" {
			if action.Method == "" {
				errs = append(errs, "method is required for REST actions")
			} else if !ValidHTTPMethods[action.Method] {
				errs = append(errs, fmt.Sprintf("method %q is invalid", action.Method))
			}
			if action.Path == "" {
				errs = append(errs, "path is required for REST actions")
			}
		}

		// GraphQL validation.
		if apiType == "graphql" && action.Query == "" && action.Method == "" {
			errs = append(errs, "query is required for GraphQL actions")
		}
	}

	// Param validation.
	for pname, param := range action.Params {
		if param.Type != "" && !validParamTypes[param.Type] {
			errs = append(errs, fmt.Sprintf("param %q has invalid type %q", pname, param.Type))
		}
		if param.Location != "" && !validParamLocations[param.Location] {
			errs = append(errs, fmt.Sprintf("param %q has invalid location %q", pname, param.Location))
		}
	}

	// Path param consistency: every {{.X}} in path must have a matching param with location: path.
	if action.Path != "" {
		for _, ref := range extractPathParams(action.Path) {
			param, ok := action.Params[ref]
			if !ok {
				errs = append(errs, fmt.Sprintf("path references {{.%s}} but no param %q is defined", ref, ref))
			} else if param.Location != "path" {
				errs = append(errs, fmt.Sprintf("param %q is referenced in path but has location %q instead of \"path\"", ref, param.Location))
			}
		}
	}

	return errs
}

// validateActionWarnings checks a single action for non-fatal issues.
func validateActionWarnings(_ string, action yamldef.Action) []string {
	var warns []string

	// POST with "read" category is unusual but legitimate (e.g. Notion search).
	if action.Method == "POST" && action.Risk.Category == "read" {
		warns = append(warns, fmt.Sprintf("POST method with risk category \"read\" is unusual — verify this is a read-only endpoint"))
	}

	// Orphan path params: location: path but not referenced in the path.
	if action.Path != "" {
		referencedInPath := map[string]bool{}
		for _, ref := range extractPathParams(action.Path) {
			referencedInPath[ref] = true
		}
		for pname, param := range action.Params {
			if param.Location == "path" && !referencedInPath[pname] {
				warns = append(warns, fmt.Sprintf("param %q has location \"path\" but is not referenced in path %q", pname, action.Path))
			}
		}
	}

	return warns
}

// extractVarPlaceholders returns variable names from {{.var.NAME}} placeholders.
func extractVarPlaceholders(s string) []string {
	matches := varPlaceholderRe.FindAllStringSubmatch(s, -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// extractPathParams returns param names from {{.NAME}} placeholders, excluding {{.var.NAME}}.
func extractPathParams(s string) []string {
	// First remove all {{.var.X}} so they don't match as path params.
	cleaned := varPlaceholderRe.ReplaceAllString(s, "")
	matches := pathParamRe.FindAllStringSubmatch(cleaned, -1)
	names := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		name := m[1]
		if strings.HasPrefix(name, "var.") {
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}
