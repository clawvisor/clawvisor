package imessage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindHelper_NotFound(t *testing.T) {
	a := &IMessageAdapter{}
	// With no helper binary installed, findHelper should return empty string.
	// Override HOME to prevent finding a real install.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("PATH", tmpDir)

	got := a.findHelper()
	if got != "" {
		t.Fatalf("expected empty string when helper not found, got %q", got)
	}
}

func TestFindHelper_StandardLocation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Create fake helper inside .app bundle at ~/.clawvisor/bin/
	binDir := filepath.Join(tmpDir, ".clawvisor", "bin", helperAppName, "Contents", "MacOS")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(binDir, helperBinaryName)
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	a := &IMessageAdapter{}
	got := a.findHelper()
	if got != helperPath {
		t.Errorf("got %q, want %q", got, helperPath)
	}
}

func TestFindHelper_CachedPath(t *testing.T) {
	tmpDir := t.TempDir()
	appBinDir := filepath.Join(tmpDir, helperAppName, "Contents", "MacOS")
	if err := os.MkdirAll(appBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(appBinDir, helperBinaryName)
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	a := &IMessageAdapter{helperPath: helperPath}
	got := a.findHelper()
	if got != helperPath {
		t.Errorf("got %q, want %q", got, helperPath)
	}
}

func TestFindHelper_CachedPathStale(t *testing.T) {
	a := &IMessageAdapter{helperPath: "/nonexistent/Clawvisor iMessage Helper.app/Contents/MacOS/clawvisor-imessage-helper"}
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("PATH", tmpDir)

	got := a.findHelper()
	if got != "" {
		t.Errorf("expected empty string for stale cached path, got %q", got)
	}
	if a.helperPath != "" {
		t.Errorf("expected helperPath to be cleared, got %q", a.helperPath)
	}
}
