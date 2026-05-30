package lite

import (
	"path/filepath"
	"testing"
)

// TestLoadAllLibraryScenarios sanity-checks every YAML under library/
// parses cleanly under strict decoding. Catches typos in field names
// before the (expensive) LLM-driven matrix runs.
func TestLoadAllLibraryScenarios(t *testing.T) {
	for _, name := range []string{
		"broad_single_scope",
		"non_default_cli",
		"pure_inspection",
		"scope_drift_followup",
		"pivot_mid_execution",
		"denied_then_explain",
		"incremental_within_scope",
		"bug_fix_workflow",
		"clarification_then_task",
		"cross_file_inspection",
		"credential_handle_discovery",
		"credential_handle_discovery_unaided",
		"no_invented_placeholder",
		"credential_not_needed_for_local",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			if _, err := LoadScenario(filepath.Join("library", name)); err != nil {
				t.Fatalf("LoadScenario(%s): %v", name, err)
			}
		})
	}
}
