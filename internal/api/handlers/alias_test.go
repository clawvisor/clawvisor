package handlers

import "testing"

func TestValidAlias(t *testing.T) {
	tests := []struct {
		alias string
		want  bool
	}{
		{"", true},
		{"default", true},
		{"work", true},
		{"my-alias", true},
		{"my_alias", true},
		{"Work123", true},
		// Email-style aliases (auto-detected identity)
		{"levine.eric.j@gmail.com", true},
		{"user+tag@example.com", true},
		{"alice@corp.co", true},
		// GitHub-style aliases
		{"octocat", true},
		// Workspace-style aliases (auto-detected identity)
		{"YC P2026", true},
		{"My Workspace", true},
		// Disallowed
		{"has/slash", false},
		{"has:colon", false},
		{"has;semi", false},
	}
	for _, tt := range tests {
		if got := validAlias(tt.alias); got != tt.want {
			t.Errorf("validAlias(%q) = %v, want %v", tt.alias, got, tt.want)
		}
	}
}
