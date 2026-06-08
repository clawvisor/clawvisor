package placeholdershape

import (
	"reflect"
	"testing"
)

// TestFindAllAutovault_TightExtraction pins the anchored extraction:
// FindAllAutovault must return the placeholder substring only, never
// the placeholder glued to surrounding context. A previous version of
// the regex allowed `[A-Za-z0-9._:-]*` as an optional prefix and
// silently corrupted extraction-path callers (audit-row placeholder
// lists and autovault/swap's resolve(candidate)).
func TestFindAllAutovault_TightExtraction(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "bare placeholder",
			in:   "autovault_gh_personal",
			want: []string{"autovault_gh_personal"},
		},
		{
			name: "placeholder with token-alphabet prefix (the bug shape)",
			in:   "xxxautovault_gh_personal",
			want: []string{"autovault_gh_personal"},
		},
		{
			name: "placeholder in Bearer header",
			in:   "Authorization: Bearer autovault_gh_personal_xyz",
			want: []string{"autovault_gh_personal_xyz"},
		},
		{
			name: "two placeholders separated by whitespace",
			in:   "autovault_a_one autovault_b_two",
			want: []string{"autovault_a_one", "autovault_b_two"},
		},
		{
			name: "no placeholder",
			in:   "no credential here",
			want: nil,
		},
		{
			name: "underscore separator in body",
			in:   "autovault_google_gmail_eric_clawvisor_com_q02r9WwdQtkcZMgF",
			want: []string{"autovault_google_gmail_eric_clawvisor_com_q02r9WwdQtkcZMgF"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FindAllAutovault(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FindAllAutovault(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestContainsAutovault_DetectionUnchanged confirms that anchoring the
// regex didn't break detection-flavored callers: any string carrying
// the literal `autovault<body>` should still match.
func TestContainsAutovault_DetectionUnchanged(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"bare", "autovault_x", true},
		{"prefixed (still detects)", "xxxautovault_x", true},
		{"in bearer", "Bearer autovault_x", true},
		{"no placeholder", "no creds", false},
		{"prefix only, no body", "autovault", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainsAutovaultString(tc.in); got != tc.want {
				t.Errorf("ContainsAutovaultString(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if got := ContainsAutovault([]byte(tc.in)); got != tc.want {
				t.Errorf("ContainsAutovault(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
