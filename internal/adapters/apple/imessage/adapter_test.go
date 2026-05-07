package imessage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/version"
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

// TestDownloadHelper_RefusesWithoutPinnedSHA proves that downloads for
// platforms without a pinned helper SHA are refused before any network
// connection is opened. This is the regression guard against running an
// unverified helper as the user.
func TestDownloadHelper_RefusesWithoutPinnedSHA(t *testing.T) {
	prev := version.IMessageHelperSHAs
	version.IMessageHelperSHAs = map[string]string{}
	t.Cleanup(func() { version.IMessageHelperSHAs = prev })

	a := &IMessageAdapter{}
	_, err := a.downloadHelper(t.TempDir())
	if err == nil {
		t.Fatalf("expected refusal when no helper SHA is pinned in the build")
	}
	if !strings.Contains(err.Error(), "no pinned helper SHA") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestIMessageHelperSHA_ReadsPinnedMap(t *testing.T) {
	prev := version.IMessageHelperSHAs
	t.Cleanup(func() { version.IMessageHelperSHAs = prev })

	version.IMessageHelperSHAs = map[string]string{
		"darwin/arm64": "abc",
		"darwin/amd64": "def",
	}
	if got := version.IMessageHelperSHA("darwin/arm64"); got != "abc" {
		t.Errorf("darwin/arm64: got %q want abc", got)
	}
	if got := version.IMessageHelperSHA("darwin/amd64"); got != "def" {
		t.Errorf("darwin/amd64: got %q want def", got)
	}
	if got := version.IMessageHelperSHA("linux/amd64"); got != "" {
		t.Errorf("linux/amd64 should be unset, got %q", got)
	}
}

// TestIMessageHelperPin_PopulatedForDarwin proves the source-pinned values
// are present for the platforms iMessage actually supports — a guard against
// an empty pin sneaking past code review and breaking self-host installs.
func TestIMessageHelperPin_PopulatedForDarwin(t *testing.T) {
	if version.IMessageHelperReleaseTag == "" {
		t.Fatal("IMessageHelperReleaseTag is empty — set it in pkg/version/imessage_helper.go")
	}
	for _, osArch := range []string{"darwin/arm64", "darwin/amd64"} {
		if got := version.IMessageHelperSHA(osArch); got == "" {
			t.Errorf("no pinned SHA for %s", osArch)
		}
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
