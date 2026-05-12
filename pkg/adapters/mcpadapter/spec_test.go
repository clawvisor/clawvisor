package mcpadapter

import "testing"

// TestExtractField covers the array indexing additions made for Notion's
// notion-get-users tool, which returns {results:[{email,...}]} — the old
// dot-only path couldn't reach `results[0].email`.
func TestExtractField(t *testing.T) {
	notionShape := map[string]any{
		"has_more": false,
		"results": []any{
			map[string]any{"email": "eric@example.com", "name": "Eric"},
			map[string]any{"email": "other@example.com"},
		},
	}
	gmailShape := map[string]any{
		"emailAddress": "x@gmail.com",
		"profile": map[string]any{
			"displayName": "X",
		},
	}

	cases := []struct {
		name string
		v    any
		path string
		want string
	}{
		{"simple top-level string", gmailShape, "emailAddress", "x@gmail.com"},
		{"nested map", gmailShape, "profile.displayName", "X"},
		{"array bracket notation", notionShape, "results[0].email", "eric@example.com"},
		{"array dot-number notation", notionShape, "results.0.email", "eric@example.com"},
		{"array second index", notionShape, "results[1].email", "other@example.com"},
		{"empty path returns string itself", "literal", "", "literal"},
		{"missing key", gmailShape, "missing", ""},
		{"missing array index", notionShape, "results[99].email", ""},
		{"non-numeric index on array", notionShape, "results.x.email", ""},
		{"path into non-map non-array", gmailShape, "emailAddress.subfield", ""},
		{"target is not a string", notionShape, "has_more", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractField(tc.v, tc.path)
			if got != tc.want {
				t.Errorf("extractField(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
