package definitions

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"gopkg.in/yaml.v3"
)

func TestGoogleGmailModifyLabelsActionDefinition(t *testing.T) {
	contents, err := FS.ReadFile("google_gmail.yaml")
	if err != nil {
		t.Fatalf("read Gmail definition: %v", err)
	}

	var definition yamldef.ServiceDef
	if err := yaml.Unmarshal(contents, &definition); err != nil {
		t.Fatalf("parse Gmail definition: %v", err)
	}
	action, ok := definition.Actions["modify_labels"]
	if !ok {
		t.Fatal("Gmail catalog is missing modify_labels")
	}
	if action.Override != "go" {
		t.Errorf("override = %q, want go", action.Override)
	}
	if action.Risk.Category != "write" || action.Risk.Sensitivity != "low" {
		t.Errorf("risk = %+v, want write/low", action.Risk)
	}
	if len(action.Scopes) != 1 || action.Scopes[0] != "https://www.googleapis.com/auth/gmail.modify" {
		t.Errorf("scopes = %v, want gmail.modify", action.Scopes)
	}
	if !action.Params["message_id"].Required || action.Params["message_id"].Type != "string" {
		t.Errorf("message_id schema = %+v, want required string", action.Params["message_id"])
	}
	for _, name := range []string{"add_label_ids", "remove_label_ids"} {
		if param, ok := action.Params[name]; !ok || param.Type != "array" {
			t.Errorf("%s schema = %+v, want array", name, param)
		}
	}
}
