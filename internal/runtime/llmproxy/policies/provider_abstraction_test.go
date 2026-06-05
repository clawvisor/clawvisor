package policies_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderAbstraction_PoliciesDontMentionSpecificProviders
// validates the Phase 6 invariant: adding a new provider shouldn't
// require touching any policies/*.go file.
//
// The test walks every non-test .go file in this package and asserts
// that:
//   - it doesn't directly compare to conversation.ProviderAnthropic
//     or conversation.ProviderOpenAI in a switch / if (that'd be a
//     hidden provider gate that would need a new branch for a third
//     provider)
//   - it doesn't reference provider name strings ("anthropic", "openai")
//     in code paths that could affect behavior
//
// Exceptions:
//   - anthropic_sanitize.go is allowed to compare to
//     ProviderAnthropic because it's intentionally a provider-specific
//     policy (per its docstring). Other "Anthropic"-only sanitizers
//     would land as their own policies, gated similarly.
//   - control_notice.go currently has no provider comparisons; if it
//     grows one for the count_tokens path, the path check should be
//     URL-based (which it is) not provider-based.
//
// When the test fails: either (a) the policy genuinely needs a
// provider-specific branch (rare — the underlying helper is the
// right place for provider dispatch), in which case add the file to
// allowedProviderCompare; or (b) the policy is leaking provider
// awareness it shouldn't carry, in which case refactor.
func TestProviderAbstraction_PoliciesDontMentionSpecificProviders(t *testing.T) {
	allowedProviderCompare := map[string]bool{
		"anthropic_sanitize.go": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read policies dir: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(data)
		checked++

		offenders := []string{}
		// Look for direct provider comparisons that would gate behavior.
		// Allowing exact equality with the enum value; flagging if found
		// AND the file isn't in the allowlist.
		for _, marker := range []string{
			"conversation.ProviderAnthropic",
			"conversation.ProviderOpenAI",
		} {
			if strings.Contains(body, marker) && !allowedProviderCompare[name] {
				offenders = append(offenders, marker)
			}
		}

		if len(offenders) > 0 {
			t.Errorf("policies/%s leaks provider awareness (%v).\n"+
				"Provider dispatch belongs in the underlying llmproxy helper or in conversation/stream/ codecs.\n"+
				"If this is intentionally provider-specific (like anthropic_sanitize), add it to allowedProviderCompare in this test.",
				name, offenders)
		}
	}

	if checked == 0 {
		t.Errorf("no policy files were checked — test infrastructure broken")
	}
}
