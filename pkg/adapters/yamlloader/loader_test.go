package yamlloader

import (
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
		"stripe":           false,
		"github":           false,
		"slack":            false,
		"twilio":           false,
		"notion":           false,
		"linear":           false,
		"google.gmail":     false,
		"google.calendar":  false,
		"google.drive":     false,
		"google.contacts":  false,
		"dropbox":          false,
		"granola":          false,
		"perplexity":       false,
		"supabase":         false,
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
