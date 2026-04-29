package policy

import "testing"

func TestDetectStarterProfileExplicitOverrideBeatsCommandDetection(t *testing.T) {
	commandKey, profileID := DetectStarterProfile("codex", []string{"claude"})
	if commandKey != "codex" || profileID != "codex" {
		t.Fatalf("DetectStarterProfile override = (%q, %q), want (%q, %q)", commandKey, profileID, "codex", "codex")
	}
}

func TestDetectStarterProfileCommandDetectionFallsBackToExecutableName(t *testing.T) {
	commandKey, profileID := DetectStarterProfile("", []string{"/usr/local/bin/claude"})
	if commandKey != "claude" || profileID != "claude_code" {
		t.Fatalf("DetectStarterProfile command detection = (%q, %q), want (%q, %q)", commandKey, profileID, "claude", "claude_code")
	}
}
