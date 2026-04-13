package credential

import "testing"

func TestMissingScopes(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		required []string
		want     []string
	}{
		{
			name:     "all present",
			existing: []string{"a", "b", "c"},
			required: []string{"a", "b"},
			want:     nil,
		},
		{
			name:     "some missing",
			existing: []string{"a"},
			required: []string{"a", "b", "c"},
			want:     []string{"b", "c"},
		},
		{
			name:     "all missing",
			existing: nil,
			required: []string{"a", "b"},
			want:     []string{"a", "b"},
		},
		{
			name:     "no required",
			existing: []string{"a"},
			required: nil,
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MissingScopes(tc.existing, tc.required)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d]=%q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHasAllScopes(t *testing.T) {
	if !HasAllScopes([]string{"a", "b", "c"}, []string{"a", "b"}) {
		t.Fatal("expected true when all required present")
	}
	if HasAllScopes([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("expected false when scope missing")
	}
}
