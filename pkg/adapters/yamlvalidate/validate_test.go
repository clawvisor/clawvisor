package yamlvalidate

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// minimalValidDef returns a minimal valid ServiceDef for testing.
func minimalValidDef() yamldef.ServiceDef {
	return yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID:          "test",
			DisplayName: "Test Service",
		},
		Auth: yamldef.AuthDef{Type: "api_key"},
		API:  yamldef.APIDef{BaseURL: "https://api.test.com", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_items": {
				DisplayName: "List items",
				Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "low", Description: "List items"},
				Method:      "GET",
				Path:        "/items",
			},
		},
	}
}

func TestValidate_MinimalValid(t *testing.T) {
	def := minimalValidDef()
	r := Validate(&def)
	if !r.OK() {
		t.Errorf("expected no errors, got: %v", r.Errors)
	}
	if len(r.Warnings) > 0 {
		t.Errorf("expected no warnings, got: %v", r.Warnings)
	}
}

func TestValidate_MissingServiceID(t *testing.T) {
	def := minimalValidDef()
	def.Service.ID = ""
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for missing service.id")
	}
	if !containsStr(r.Errors, "service.id is required") {
		t.Errorf("expected 'service.id is required' error, got: %v", r.Errors)
	}
}

func TestValidate_InvalidServiceID(t *testing.T) {
	def := minimalValidDef()
	def.Service.ID = "HAS SPACES"
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for invalid service.id")
	}
	if !containsStr(r.Errors, "invalid characters") {
		t.Errorf("expected 'invalid characters' error, got: %v", r.Errors)
	}
}

func TestValidate_MissingDisplayName(t *testing.T) {
	def := minimalValidDef()
	def.Service.DisplayName = ""
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for missing display_name")
	}
}

func TestValidate_InvalidAuthType(t *testing.T) {
	def := minimalValidDef()
	def.Auth.Type = "magic"
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for invalid auth type")
	}
}

func TestValidate_InvalidAPIType(t *testing.T) {
	def := minimalValidDef()
	def.API.Type = "soap"
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for invalid api.type")
	}
}

func TestValidate_MissingBaseURL(t *testing.T) {
	def := minimalValidDef()
	def.API.BaseURL = ""
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for missing base_url")
	}
}

func TestValidate_NoActions(t *testing.T) {
	def := minimalValidDef()
	def.Actions = map[string]yamldef.Action{}
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for zero actions")
	}
}

// ── Variable checks ─────────────────────────────────────────────────────────

func TestValidate_VarReferencedButNotDefined(t *testing.T) {
	def := minimalValidDef()
	def.API.BaseURL = "{{.var.instance_url}}/api"
	// No variables defined.
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for undefined variable reference")
	}
	if !containsStr(r.Errors, "instance_url") {
		t.Errorf("expected error mentioning 'instance_url', got: %v", r.Errors)
	}
}

func TestValidate_VarDefinedAndReferenced(t *testing.T) {
	def := minimalValidDef()
	def.API.BaseURL = "{{.var.instance_url}}/api"
	def.Variables = map[string]yamldef.VariableDef{
		"instance_url": {DisplayName: "Instance URL", Required: true},
	}
	r := Validate(&def)
	if !r.OK() {
		t.Errorf("expected no errors, got: %v", r.Errors)
	}
}

func TestValidate_OrphanVariable(t *testing.T) {
	def := minimalValidDef()
	def.Variables = map[string]yamldef.VariableDef{
		"unused_var": {DisplayName: "Unused"},
	}
	r := Validate(&def)
	if !r.OK() {
		t.Errorf("orphan variable should be warning not error, got errors: %v", r.Errors)
	}
	if !containsStr(r.Warnings, "unused_var") {
		t.Errorf("expected warning about orphan variable, got: %v", r.Warnings)
	}
}

func TestValidate_RequiredVarNoDisplayName(t *testing.T) {
	def := minimalValidDef()
	def.API.BaseURL = "{{.var.site}}/api"
	def.Variables = map[string]yamldef.VariableDef{
		"site": {Required: true}, // no display_name
	}
	r := Validate(&def)
	if !containsStr(r.Warnings, "display_name") {
		t.Errorf("expected warning about missing display_name, got: %v", r.Warnings)
	}
}

// ── Path param checks ───────────────────────────────────────────────────────

func TestValidate_PathParamNotDefined(t *testing.T) {
	def := minimalValidDef()
	def.Actions["get_item"] = yamldef.Action{
		DisplayName: "Get item",
		Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "low", Description: "Get item"},
		Method:      "GET",
		Path:        "/items/{{.item_id}}",
		// No params defined.
	}
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for missing path param")
	}
	if !containsStr(r.Errors, "item_id") {
		t.Errorf("expected error mentioning 'item_id', got: %v", r.Errors)
	}
}

func TestValidate_PathParamWrongLocation(t *testing.T) {
	def := minimalValidDef()
	def.Actions["get_item"] = yamldef.Action{
		DisplayName: "Get item",
		Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "low", Description: "Get item"},
		Method:      "GET",
		Path:        "/items/{{.item_id}}",
		Params: map[string]yamldef.Param{
			"item_id": {Type: "string", Location: "query"}, // wrong location
		},
	}
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for path param with wrong location")
	}
	if !containsStr(r.Errors, "\"path\"") {
		t.Errorf("expected error about location, got: %v", r.Errors)
	}
}

func TestValidate_PathParamCorrect(t *testing.T) {
	def := minimalValidDef()
	def.Actions["get_item"] = yamldef.Action{
		DisplayName: "Get item",
		Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "low", Description: "Get item"},
		Method:      "GET",
		Path:        "/items/{{.item_id}}",
		Params: map[string]yamldef.Param{
			"item_id": {Type: "string", Required: true, Location: "path"},
		},
	}
	r := Validate(&def)
	if !r.OK() {
		t.Errorf("expected no errors, got: %v", r.Errors)
	}
}

func TestValidate_OrphanPathParam(t *testing.T) {
	def := minimalValidDef()
	def.Actions["list_items"] = yamldef.Action{
		DisplayName: "List items",
		Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "low", Description: "List items"},
		Method:      "GET",
		Path:        "/items",
		Params: map[string]yamldef.Param{
			"orphan": {Type: "string", Location: "path"}, // location: path but not in path
		},
	}
	r := Validate(&def)
	if !containsStr(r.Warnings, "orphan") {
		t.Errorf("expected warning about orphan path param, got: %v", r.Warnings)
	}
}

// ── Param type/location checks ──────────────────────────────────────────────

func TestValidate_InvalidParamType(t *testing.T) {
	def := minimalValidDef()
	a := def.Actions["list_items"]
	a.Params = map[string]yamldef.Param{"bad": {Type: "float", Location: "query"}}
	def.Actions["list_items"] = a

	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for invalid param type")
	}
	if !containsStr(r.Errors, "float") {
		t.Errorf("expected error mentioning 'float', got: %v", r.Errors)
	}
}

func TestValidate_InvalidParamLocation(t *testing.T) {
	def := minimalValidDef()
	a := def.Actions["list_items"]
	a.Params = map[string]yamldef.Param{"bad": {Type: "string", Location: "header"}}
	def.Actions["list_items"] = a

	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for invalid param location")
	}
	if !containsStr(r.Errors, "header") {
		t.Errorf("expected error mentioning 'header', got: %v", r.Errors)
	}
}

// ── Risk sanity checks ──────────────────────────────────────────────────────

func TestValidate_DeleteMethodReadCategory(t *testing.T) {
	def := minimalValidDef()
	def.Actions["bad_delete"] = yamldef.Action{
		DisplayName: "Bad delete",
		Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "high", Description: "Bad"},
		Method:      "DELETE",
		Path:        "/items/{{.id}}",
		Params:      map[string]yamldef.Param{"id": {Type: "string", Required: true, Location: "path"}},
	}
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for DELETE with read category")
	}
}

func TestValidate_DeleteMethodLowSensitivity(t *testing.T) {
	def := minimalValidDef()
	def.Actions["bad_delete"] = yamldef.Action{
		DisplayName: "Bad delete",
		Risk:        yamldef.RiskDef{Category: "delete", Sensitivity: "low", Description: "Bad"},
		Method:      "DELETE",
		Path:        "/items/{{.id}}",
		Params:      map[string]yamldef.Param{"id": {Type: "string", Required: true, Location: "path"}},
	}
	r := Validate(&def)
	if r.OK() {
		t.Fatal("expected error for DELETE with low sensitivity")
	}
}

// ── Full Jira-like def ──────────────────────────────────────────────────────

func TestValidate_JiraLikeDef(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID:          "jira",
			DisplayName: "Jira Cloud",
			Description: "Manage Jira issues.",
			Identity:    &yamldef.IdentityDef{Endpoint: "/rest/api/3/myself", Field: "emailAddress"},
		},
		Auth: yamldef.AuthDef{Type: "basic"},
		API:  yamldef.APIDef{BaseURL: "{{.var.instance_url}}", Type: "rest"},
		Variables: map[string]yamldef.VariableDef{
			"instance_url": {
				DisplayName: "Instance URL",
				Description: "Your Atlassian Cloud URL",
				Required:    true,
			},
		},
		Actions: map[string]yamldef.Action{
			"search_issues": {
				DisplayName: "Search issues",
				Risk:        yamldef.RiskDef{Category: "search", Sensitivity: "low", Description: "Search issues"},
				Method:      "GET",
				Path:        "/rest/api/3/search",
				Params: map[string]yamldef.Param{
					"jql": {Type: "string", Required: true, Location: "query"},
				},
			},
			"get_issue": {
				DisplayName: "Get issue",
				Risk:        yamldef.RiskDef{Category: "read", Sensitivity: "low", Description: "Get issue"},
				Method:      "GET",
				Path:        "/rest/api/3/issue/{{.issue_key}}",
				Params: map[string]yamldef.Param{
					"issue_key": {Type: "string", Required: true, Location: "path"},
				},
			},
		},
	}
	r := Validate(&def)
	if !r.OK() {
		t.Errorf("expected Jira-like def to be valid, got errors: %v", r.Errors)
	}
	if len(r.Warnings) > 0 {
		t.Errorf("expected no warnings, got: %v", r.Warnings)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func containsStr(ss []string, substr string) bool {
	for _, s := range ss {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}
