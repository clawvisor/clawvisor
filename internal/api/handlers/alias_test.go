package handlers

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

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

func TestClosestParamName(t *testing.T) {
	defs := []adapters.ParamInfo{
		{
			Name:    "to",
			Type:    "string",
			Aliases: []string{"time_max"},
		},
		{
			Name:    "from",
			Type:    "string",
			Aliases: []string{"time_min"},
		},
	}

	tests := []struct {
		input string
		want  string
	}{
		{"to", "to"},
		{"time_max", "time_max"},
		{"time_mx", "time_max"}, // typo of alias
		{"time_mn", "time_min"}, // typo of alias
		{"t", ""},
		{"unrelated", ""},
	}

	for _, tt := range tests {
		got := closestParamName(tt.input, defs)
		if got != tt.want {
			t.Errorf("closestParamName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
