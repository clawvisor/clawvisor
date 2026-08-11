package handlers

import "testing"

func TestFlattenParamDesc(t *testing.T) {
	// YAML ">" folds but can leave a trailing newline; "|" preserves them
	// outright. Either would break out of the markdown list item.
	cases := map[string]string{
		"simple":                     "simple",
		"folded across\nlines\n":     "folded across lines",
		"literal\n  block\n  text\n": "literal block text",
		"  leading and trailing  ":   "leading and trailing",
		"":                           "",
	}
	for in, want := range cases {
		if got := flattenParamDesc(in); got != want {
			t.Errorf("flattenParamDesc(%q) = %q, want %q", in, got, want)
		}
	}
}
