package yamlloader

import (
	"slices"
	"testing"

	"github.com/clawvisor/clawvisor/internal/adapters/definitions"
)

func TestLoadEmbeddedDefinitions(t *testing.T) {
	loader := New(definitions.FS, nil, nil, nil)
	if err := loader.LoadAll(); err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	adapters := loader.Adapters()
	if len(adapters) == 0 {
		t.Fatal("expected at least one adapter")
	}

	// Verify all expected services are loaded.
	expected := map[string]bool{
		"stripe":             false,
		"github":             false,
		"slack":              false,
		"twilio":             false,
		"notion":             false,
		"linear":             false,
		"google.gmail":       false,
		"google.calendar":    false,
		"google.drive":       false,
		"google.contacts":    false,
		"google.sheets":      false,
		"dropbox":            false,
		"granola":            false,
		"perplexity":         false,
		"microsoft.onedrive": false,
		"microsoft.outlook":  false,
	}

	for _, a := range adapters {
		id := a.ServiceID()
		if _, ok := expected[id]; ok {
			expected[id] = true
		} else {
			t.Errorf("unexpected service ID: %q", id)
		}
	}

	for id, found := range expected {
		if !found {
			t.Errorf("expected service %q not loaded", id)
		}
	}
}

func TestLoadedDefinitionsHaveMetadata(t *testing.T) {
	loader := New(definitions.FS, nil, nil, nil)
	if err := loader.LoadAll(); err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	for _, a := range loader.Adapters() {
		meta := a.ServiceMetadata()
		if meta.DisplayName == "" {
			t.Errorf("service %q: missing display name", a.ServiceID())
		}
		if meta.Description == "" {
			t.Errorf("service %q: missing description", a.ServiceID())
		}
		if len(meta.ActionMeta) == 0 {
			t.Errorf("service %q: no action metadata", a.ServiceID())
		}

		// Verify each action has risk metadata.
		for actionName, am := range meta.ActionMeta {
			if am.DisplayName == "" {
				t.Errorf("service %q action %q: missing display name", a.ServiceID(), actionName)
			}
			if am.Category == "" {
				t.Errorf("service %q action %q: missing risk category", a.ServiceID(), actionName)
			}
			if am.Sensitivity == "" {
				t.Errorf("service %q action %q: missing risk sensitivity", a.ServiceID(), actionName)
			}
		}
	}
}

// Guards against a max_bytes block landing in the wrong action — it is easy to
// misplace in YAML and the failure is silent: the adapter falls back to the
// default limit and callers can never opt into a larger download.
func TestDownloadActionsExposeMaxBytes(t *testing.T) {
	loader := New(definitions.FS, nil, nil, nil)
	if err := loader.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	defs := map[string]map[string][]string{} // service -> action -> param names
	for _, a := range loader.Adapters() {
		actions := map[string][]string{}
		for name, action := range a.Def().Actions {
			var params []string
			for p := range action.Params {
				params = append(params, p)
			}
			actions[name] = params
		}
		defs[a.ServiceID()] = actions
	}

	want := map[string][]string{
		"dropbox":            {"download_file"},
		"google.drive":       {"download_file", "export_file"},
		"microsoft.onedrive": {"download_file"},
		"slack":              {"download_file"},
		"google.gmail":       {"get_message_raw"},
	}
	for service, wantActions := range want {
		actions, ok := defs[service]
		if !ok {
			t.Errorf("service %q did not load", service)
			continue
		}
		for _, action := range wantActions {
			params, ok := actions[action]
			if !ok {
				t.Errorf("%s: action %q missing", service, action)
				continue
			}
			if !slices.Contains(params, "max_bytes") {
				t.Errorf("%s %s: missing max_bytes param, has %v", service, action, params)
			}
		}
	}

	// And it must not leak into unrelated actions — the failure mode is
	// silent, so assert the exact set.
	for service, actions := range defs {
		for action, params := range actions {
			if slices.Contains(params, "max_bytes") && !slices.Contains(want[service], action) {
				t.Errorf("%s %s: unexpected max_bytes param", service, action)
			}
		}
	}
}
