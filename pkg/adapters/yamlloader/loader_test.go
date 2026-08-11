package yamlloader

import (
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/adapters/definitions"
	"gopkg.in/yaml.v3"
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

// Param descriptions were silently discarded for the life of the schema:
// yamldef.Param had no Description field, so yaml.v3 dropped every
// `description:` written under a param. Definitions carried 46 of them across
// 8 services, all documenting nothing.
func TestParamDescriptionsSurviveParsing(t *testing.T) {
	loader := New(definitions.FS, nil, nil, nil)
	if err := loader.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	described := 0
	gotPerService := map[string]int{}
	for _, a := range loader.Adapters() {
		for actionName, action := range a.Def().Actions {
			for name, p := range action.Params {
				if p.Description == "" {
					continue
				}
				described++
				gotPerService[a.ServiceID()]++
				// Must reach both metadata carriers, which feed the catalog
				// and pre-execution validation respectively.
				var viaInfo string
				for _, pi := range a.ActionParams(actionName) {
					if pi.Name == name {
						viaInfo = pi.Description
					}
				}
				if viaInfo != p.Description {
					t.Errorf("%s %s %s: ActionParams description = %q, want %q",
						a.ServiceID(), actionName, name, viaInfo, p.Description)
				}
				var viaMeta string
				for _, pm := range a.ServiceMetadata().ActionMeta[actionName].Params {
					if pm.Name == name {
						viaMeta = pm.Description
					}
				}
				if viaMeta != p.Description {
					t.Errorf("%s %s %s: ParamMeta description = %q, want %q",
						a.ServiceID(), actionName, name, viaMeta, p.Description)
				}
			}
		}
	}

	// Guard the guard. A floor like "> 20" would still pass if an entire
	// service lost its docs — those params simply never enter the loop above,
	// so every assertion passes vacuously. Instead, count what the YAML
	// actually declares by parsing it generically, and require the typed
	// schema to have captured exactly that. Self-maintaining: adding or
	// removing a description in a definition needs no test edit, but dropping
	// one on the floor fails.
	wantPerService := rawParamDescriptionCounts(t)
	if !reflect.DeepEqual(gotPerService, wantPerService) {
		t.Errorf("param descriptions captured per service = %v, want %v (from raw YAML)",
			gotPerService, wantPerService)
	}
	t.Logf("verified %d param descriptions reach both metadata paths", described)
}

// rawParamDescriptionCounts parses every embedded definition into a generic
// map — bypassing yamldef entirely — and counts param-level descriptions per
// service. This is the ground truth the typed schema is checked against.
func rawParamDescriptionCounts(t *testing.T) map[string]int {
	t.Helper()
	entries, err := fs.ReadDir(definitions.FS, ".")
	if err != nil {
		t.Fatalf("reading definitions: %v", err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := fs.ReadFile(definitions.FS, e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			continue
		}
		svc, _ := doc["service"].(map[string]any)
		id, _ := svc["id"].(string)
		actions, _ := doc["actions"].(map[string]any)
		n := 0
		for _, a := range actions {
			am, _ := a.(map[string]any)
			params, _ := am["params"].(map[string]any)
			for _, p := range params {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if d, _ := pm["description"].(string); strings.TrimSpace(d) != "" {
					n++
				}
			}
		}
		if n > 0 {
			counts[id] = n
		}
	}
	return counts
}
