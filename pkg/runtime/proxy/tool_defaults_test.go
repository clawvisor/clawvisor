package proxy

import (
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestAllowSessionScopedToolDefaultAllowsWorkspaceRead(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	reason, ok := allowSessionScopedToolDefault(session, "Read", map[string]any{"file_path": "README.md"}, true)
	if !ok {
		t.Fatal("expected workspace read to be allowed")
	}
	if reason == "" {
		t.Fatal("expected allow reason")
	}
}

func TestAllowSessionScopedToolDefaultAllowsTmpWrite(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Write", map[string]any{"file_path": "/tmp/demo.txt"}, true); !ok {
		t.Fatal("expected /tmp write to be allowed")
	}
}

func TestAllowSessionScopedToolDefaultBlocksOutsideRoots(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Read", map[string]any{"file_path": "/etc/passwd"}, true); ok {
		t.Fatal("expected outside-root read to require approval")
	}
}

func TestAllowSessionScopedToolDefaultAllowsRelativeSearch(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Glob", map[string]any{"pattern": "**/*.go"}, true); !ok {
		t.Fatal("expected relative glob search to be allowed")
	}
}

func TestAllowSessionScopedToolDefaultBlocksAbsoluteSearchOutsideRoots(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Glob", map[string]any{"pattern": "/etc/**/*.conf"}, true); ok {
		t.Fatal("expected absolute glob outside roots to require approval")
	}
}

func TestAllowSessionScopedToolDefaultGatesSensitiveReadInsideWorkspace(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Read", map[string]any{"file_path": ".env"}, true); ok {
		t.Fatal("expected .env in workspace to fall through to task scope / approval")
	}
	if _, ok := allowSessionScopedToolDefault(session, "Read", map[string]any{"file_path": "secrets/service-account-prod.json"}, true); ok {
		t.Fatal("expected service-account key to fall through")
	}
}

func TestAllowSessionScopedToolDefaultStillAllowsEnvTemplateRead(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Read", map[string]any{"file_path": ".env.example"}, true); !ok {
		t.Fatal("expected .env.example template to remain auto-allowed")
	}
}

func TestAllowSessionScopedToolDefaultAllowsWritingSensitivePath(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Write", map[string]any{"file_path": ".env"}, true); !ok {
		t.Fatal("write to .env should still be allowed (the gate is on reads, not writes)")
	}
}

func TestAllowSessionScopedToolDefaultGatesSearchOverSensitiveDir(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Grep", map[string]any{"path": ".ssh"}, true); ok {
		t.Fatal("expected grep over a .ssh dir to fall through")
	}
}

func TestAllowSessionScopedToolDefaultSkipsSensitiveCheckWhenGuardDisabled(t *testing.T) {
	session := runtimeSessionWithToolRoots(t, "/repo/app", []string{"/repo/app", "/tmp"})
	if _, ok := allowSessionScopedToolDefault(session, "Read", map[string]any{"file_path": ".env"}, false); !ok {
		t.Fatal("expected .env read to auto-allow when guard is disabled")
	}
	if _, ok := allowSessionScopedToolDefault(session, "Grep", map[string]any{"path": ".ssh"}, false); !ok {
		t.Fatal("expected grep over .ssh to auto-allow when guard is disabled")
	}
}

func TestSummarizeToolUseIncludesPathContext(t *testing.T) {
	got := summarizeToolUse("Read", map[string]any{"file_path": "/tmp/demo.txt"})
	if got != "Read /tmp/demo.txt" {
		t.Fatalf("summarizeToolUse = %q, want %q", got, "Read /tmp/demo.txt")
	}
}

func runtimeSessionWithToolRoots(t *testing.T, workingDir string, roots []string) *store.RuntimeSession {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"starter_profile":    "claude_code",
		"working_dir":        workingDir,
		"tool_allowed_roots": roots,
	})
	if err != nil {
		t.Fatalf("Marshal(metadata): %v", err)
	}
	return &store.RuntimeSession{MetadataJSON: raw}
}
