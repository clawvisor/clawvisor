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

func TestClaudeStarterProfileIncludesObservedStartupTraffic(t *testing.T) {
	profile, ok := StarterProfileByID("claude_code")
	if !ok {
		t.Fatalf("StarterProfileByID(claude_code) = false, want true")
	}
	want := []struct {
		host   string
		path   string
		regex  string
		method string
	}{
		{host: "api.anthropic.com", path: "/v1/messages", method: "POST"},
		{host: "api.anthropic.com", path: "/api/claude_cli/bootstrap"},
		{host: "api.anthropic.com", path: "/v1/mcp_servers"},
		{host: "api.anthropic.com", regex: `^/api/eval/.*`},
		{host: "mcp-proxy.anthropic.com", regex: `^/v1/mcp/.*`},
		{host: "downloads.claude.ai", path: "/claude-code-releases/plugins/claude-plugins-official/latest", method: "GET"},
		{host: "downloads.claude.ai", regex: `^/claude-code-releases/plugins/claude-plugins-official/.*`, method: "GET"},
		{host: "storage.googleapis.com", regex: `^/claude-code-dist-[^/]+/claude-code-releases/stable$`, method: "GET"},
		{host: "http-intake.logs.us5.datadoghq.com", path: "/api/v2/logs", method: "POST"},
	}
	for _, expected := range want {
		found := false
		for _, rule := range profile.Rules {
			if rule.Kind != "egress" || rule.Action != "allow" || rule.Host != expected.host {
				continue
			}
			if expected.method != "" && rule.Method != expected.method {
				continue
			}
			if expected.path != "" && rule.Path == expected.path {
				found = true
				break
			}
			if expected.regex != "" && rule.PathRegex == expected.regex {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("claude_code starter profile missing allow rule for host=%q method=%q path=%q regex=%q", expected.host, expected.method, expected.path, expected.regex)
		}
	}
}
