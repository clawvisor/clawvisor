package main

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/tui/client"
)

func TestShouldSuppressStarterProfilePrompt(t *testing.T) {
	tests := []struct {
		name     string
		decision *client.RuntimePresetDecision
		settings *client.AgentRuntimeSettings
		profile  string
		want     bool
	}{
		{
			name: "suppresses skipped decision",
			decision: &client.RuntimePresetDecision{
				Decision: "skipped",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "suppresses always skip decision",
			decision: &client.RuntimePresetDecision{
				Decision: "always_skip",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "suppresses applied decision",
			decision: &client.RuntimePresetDecision{
				Decision: "applied",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "suppresses matching starter profile in settings",
			settings: &client.AgentRuntimeSettings{
				StarterProfile: "Codex",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "does not suppress unrelated state",
			decision: &client.RuntimePresetDecision{
				Decision: "unknown",
			},
			settings: &client.AgentRuntimeSettings{
				StarterProfile: "claude",
			},
			profile: "codex",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSuppressStarterProfilePrompt(tt.decision, tt.settings, tt.profile); got != tt.want {
				t.Fatalf("shouldSuppressStarterProfilePrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObserveModeNotice(t *testing.T) {
	got := observeModeNotice()
	want := "Clawvisor is in observe mode for this session. Actions are being analyzed and logged, but not blocked. To remove this notice, switch this agent to enforce mode in the Clawvisor dashboard."
	if got != want {
		t.Fatalf("observeModeNotice() = %q, want %q", got, want)
	}
}
