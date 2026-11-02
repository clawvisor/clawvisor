package definitions

import (
	"slices"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlvalidate"
	"gopkg.in/yaml.v3"
)

func TestGoogleCalendarVersionedActionDefinitions(t *testing.T) {
	contents, err := FS.ReadFile("google_calendar.yaml")
	if err != nil {
		t.Fatalf("read Calendar definition: %v", err)
	}

	var definition yamldef.ServiceDef
	if err := yaml.Unmarshal(contents, &definition); err != nil {
		t.Fatalf("parse Calendar definition: %v", err)
	}
	if validation := yamlvalidate.Validate(&definition); !validation.OK() {
		t.Fatalf("Calendar definition validation errors: %v", validation.Errors)
	}

	getEvent := definition.Actions["get_event"]
	if getEvent.Override != "go" {
		t.Fatalf("get_event override = %q, want go", getEvent.Override)
	}
	var responseFields []string
	for _, field := range getEvent.Response.Fields {
		responseFields = append(responseFields, field.Name)
	}
	for _, want := range []string{"etag", "version", "sequence"} {
		if !slices.Contains(responseFields, want) {
			t.Errorf("get_event response is missing %q", want)
		}
	}

	for _, actionName := range []string{"update_event", "respond_to_event"} {
		action, ok := definition.Actions[actionName]
		if !ok {
			t.Fatalf("Calendar catalog is missing %s", actionName)
		}
		if action.Override != "go" {
			t.Errorf("%s override = %q, want go", actionName, action.Override)
		}
		expected := action.Params["expected_etag"]
		if !expected.Required || !slices.Contains(expected.Aliases, "expected_version") {
			t.Errorf("%s expected_etag schema = %+v, want required with expected_version alias", actionName, expected)
		}
	}

	responseStatus := definition.Actions["respond_to_event"].Params["response_status"]
	if !responseStatus.Required || responseStatus.Type != "string" {
		t.Errorf("respond_to_event response_status schema = %+v, want required string", responseStatus)
	}
}
