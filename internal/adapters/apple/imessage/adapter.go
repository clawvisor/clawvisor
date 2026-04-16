// Package imessage implements the Clawvisor adapter for Apple iMessage.
//
// This adapter is a thin client that delegates all database operations to the
// imessage-helper binary. The helper is a separate, stable binary that holds
// Full Disk Access — because macOS ties FDA to the specific binary, keeping
// the helper separate means users don't need to re-grant FDA on every
// clawvisor update.
//
// The helper is installed on demand the first time the adapter is activated.
// It exposes a protocol version that the adapter checks — the helper binary
// is only replaced (and FDA re-granted) when the protocol actually changes.
//
// The helper communicates via JSON over stdin/stdout:
//
//	Request:  {"action":"search_messages","params":{...}}
//	Response: {"summary":"...","data":[...]}
//	Error:    {"error":"..."}
package imessage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/version"
)

const serviceID = "apple.imessage"

// helperAppName is the .app bundle that holds Full Disk Access.
const helperAppName = "Clawvisor iMessage Helper.app"

// helperBinaryName is the executable inside the .app bundle.
const helperBinaryName = "clawvisor-imessage-helper"

// helperRelBinary is the path from the .app root to the executable.
const helperRelBinary = "Contents/MacOS/clawvisor-imessage-helper"

// requiredProtocolVersion must match the helper's ProtocolVersion constant.
// Bump this when a new helper binary is needed (new actions, changed behavior).
// Bumping this will cause the adapter to download a new helper, which means
// users need to re-grant Full Disk Access.
const requiredProtocolVersion = "1"

const (
	githubOwner = "clawvisor"
	githubRepo  = "clawvisor"
)

// IMessageAdapter implements adapters.Adapter for Apple iMessage.
// It delegates all database operations to the imessage-helper binary.
type IMessageAdapter struct {
	helperPath string // resolved and validated at first use
}

func New() *IMessageAdapter {
	return &IMessageAdapter{}
}

// Available returns true if the adapter should be shown on this platform.
// iMessage only makes sense on macOS.
func (a *IMessageAdapter) Available() bool {
	return runtime.GOOS == "darwin"
}

func (a *IMessageAdapter) ServiceID() string { return serviceID }

func (a *IMessageAdapter) SupportedActions() []string {
	return []string{"search_messages", "list_threads", "get_thread", "send_message"}
}

// OAuthConfig returns nil — iMessage uses local file access, no OAuth.
func (a *IMessageAdapter) OAuthConfig() *oauth2.Config { return nil }

// RequiredScopes returns nil — iMessage uses local file access, not OAuth scopes.
func (a *IMessageAdapter) RequiredScopes() []string { return nil }

// CredentialFromToken is unused for local services.
func (a *IMessageAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("imessage: no token exchange — local service")
}

// ValidateCredential accepts any non-nil byte slice (no stored credential needed).
func (a *IMessageAdapter) ValidateCredential(_ []byte) error { return nil }

// ServiceMetadata returns display and risk metadata for iMessage.
func (a *IMessageAdapter) ServiceMetadata() adapters.ServiceMetadata {
	maxThreads := 50
	return adapters.ServiceMetadata{
		DisplayName: "iMessage",
		Description: "Search and read iMessage threads",
		IconURL:     "/logos/apple-imessage.svg",
		ActionMeta: map[string]adapters.ActionMeta{
			"search_messages": {
				DisplayName: "Search messages", Category: "search", Sensitivity: "low",
				Description: "Search iMessage history",
				Params: []adapters.ParamMeta{
					{Name: "query", Type: "string", Required: true},
					{Name: "contact", Type: "string"},
				},
			},
			"list_threads": {
				DisplayName: "List threads", Category: "read", Sensitivity: "low",
				Description: "List iMessage conversation threads",
				Params: []adapters.ParamMeta{
					{Name: "max_results", Type: "int", Default: 20, Max: &maxThreads},
				},
			},
			"get_thread": {
				DisplayName: "Get thread", Category: "read", Sensitivity: "low",
				Description: "Read a specific iMessage thread",
				Params: []adapters.ParamMeta{
					{Name: "contact", Type: "string"},
					{Name: "thread_id", Type: "string"},
				},
			},
			"send_message": {
				DisplayName: "Send message", Category: "write", Sensitivity: "high",
				Description: "Send an iMessage (requires per-request approval)",
				Params: []adapters.ParamMeta{
					{Name: "to", Type: "string", Required: true},
					{Name: "text", Type: "string", Required: true},
				},
			},
		},
	}
}

func (a *IMessageAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("imessage: only available on macOS")
	}
	resp, err := a.callHelper(ctx, req.Action, req.Params)
	if err != nil {
		return nil, err
	}
	return &adapters.Result{
		Summary: resp.Summary,
		Data:    resp.Data,
	}, nil
}

// CheckPermissions ensures the helper binary is installed (downloading it on
// demand if needed), checks its protocol version, then delegates to the
// helper's check_permissions action. This triggers macOS to register the
// helper for Full Disk Access.
func (a *IMessageAdapter) CheckPermissions() error {
	if err := a.ensureHelper(); err != nil {
		return err
	}
	resp, err := a.callHelper(context.Background(), "check_permissions", nil)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// ── On-demand helper install ─────────────────────────────────────────────────

// ensureHelper finds, version-checks, and if necessary downloads the helper
// binary. After this returns successfully, a.helperPath is set and the helper
// is at the required protocol version.
func (a *IMessageAdapter) ensureHelper() error {
	path := a.findHelper()

	if path != "" {
		// Helper exists — check protocol version.
		v, err := a.queryHelperVersion(path)
		if err == nil && v == requiredProtocolVersion {
			a.helperPath = path
			return nil
		}
		// Version mismatch or unreadable — need to replace it.
		// Fall through to download.
	}

	// Download the helper binary.
	installDir, err := helperInstallDir()
	if err != nil {
		return err
	}
	installed, err := a.downloadHelper(installDir)
	if err != nil {
		return fmt.Errorf("imessage: failed to install helper: %w", err)
	}

	a.helperPath = installed

	// Attempt to open chat.db so macOS registers the helper in the FDA list.
	// This will fail with a permission error (which is expected), but the
	// attempt is what causes the helper to appear in System Settings →
	// Privacy & Security → Full Disk Access.
	_ = a.runCheckPermissions()

	if path == "" {
		return fmt.Errorf("imessage helper installed — grant Full Disk Access to %q in System Settings → Privacy & Security → Full Disk Access", helperAppName)
	}
	// Replaced an existing binary — FDA needs re-granting.
	return fmt.Errorf("imessage helper updated (protocol version changed) — re-grant Full Disk Access to %q in System Settings → Privacy & Security → Full Disk Access", helperAppName)
}

// runCheckPermissions calls the helper's check_permissions action, which
// attempts to open chat.db. The result is ignored — the purpose is to trigger
// macOS to register the .app bundle in the FDA list.
func (a *IMessageAdapter) runCheckPermissions() error {
	reqBody, _ := json.Marshal(helperRequest{Action: "check_permissions"})
	_, err := a.execHelper(context.Background(), reqBody)
	return err
}

// helperAppDir returns the .app bundle path for the given binary path.
// If the binary is not inside a .app bundle, returns empty string.
func helperAppDir(binaryPath string) string {
	// binaryPath is like /path/to/Clawvisor iMessage Helper.app/Contents/MacOS/clawvisor-imessage-helper
	// Walk up to find the .app directory.
	dir := binaryPath
	for {
		dir = filepath.Dir(dir)
		if dir == "/" || dir == "." {
			return ""
		}
		if filepath.Ext(dir) == ".app" {
			return dir
		}
	}
}

// findHelper looks for an existing helper binary in standard locations.
// Returns the path to the binary inside the .app bundle if found, empty string otherwise.
func (a *IMessageAdapter) findHelper() string {
	// 1. Cached from a previous call.
	if a.helperPath != "" {
		if _, err := os.Stat(a.helperPath); err == nil {
			return a.helperPath
		}
		a.helperPath = ""
	}

	// 2. Next to the running binary (as .app bundle).
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), helperAppName, helperRelBinary)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// 3. Standard install location.
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".clawvisor", "bin", helperAppName, helperRelBinary)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// 4. PATH lookup (dev convenience — bare binary without .app bundle).
	if p, err := exec.LookPath(helperBinaryName); err == nil {
		return p
	}

	return ""
}

// queryHelperVersion runs the helper's "version" action and returns the
// protocol version string.
func (a *IMessageAdapter) queryHelperVersion(helperPath string) (string, error) {
	reqBody, _ := json.Marshal(helperRequest{Action: "version"})
	cmd := exec.Command(helperPath)
	cmd.Stdin = bytes.NewReader(reqBody)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			ProtocolVersion string `json:"protocol_version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", err
	}
	return resp.Data.ProtocolVersion, nil
}

// downloadHelper downloads the helper .app bundle from the GitHub release
// matching the current clawvisor version.
func (a *IMessageAdapter) downloadHelper(installDir string) (string, error) {
	tag := "v" + version.Version
	assetName := fmt.Sprintf("clawvisor-imessage-helper-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	url := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s",
		githubOwner, githubRepo, tag, assetName)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %d from %s", resp.StatusCode, url)
	}

	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", err
	}

	// Extract to a temp dir, then atomic-rename into place.
	tmpDir, err := os.MkdirTemp(installDir, ".helper-install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir) // clean up on failure

	if err := extractTarGz(resp.Body, tmpDir); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}

	// Codesign the binary inside the .app bundle.
	binaryPath := filepath.Join(tmpDir, helperAppName, helperRelBinary)
	if out, err := exec.Command("codesign", "-s", "-", binaryPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("codesign: %w — %s", err, out)
	}

	// Atomic swap: remove old .app, rename new one into place.
	destApp := filepath.Join(installDir, helperAppName)
	os.RemoveAll(destApp)
	if err := os.Rename(filepath.Join(tmpDir, helperAppName), destApp); err != nil {
		return "", err
	}

	return filepath.Join(destApp, helperRelBinary), nil
}

// extractTarGz extracts a gzipped tar archive into destDir.
func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, filepath.Clean(hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

// helperInstallDir returns ~/.clawvisor/bin, creating it if needed.
func helperInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".clawvisor", "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// ── Helper binary communication ──────────────────────────────────────────────

type helperResponse struct {
	Summary string          `json:"summary,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type helperRequest struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

func (a *IMessageAdapter) callHelper(ctx context.Context, action string, params map[string]any) (*helperResponse, error) {
	if a.helperPath == "" {
		if err := a.ensureHelper(); err != nil {
			return nil, err
		}
	}

	reqBody, err := json.Marshal(helperRequest{Action: action, Params: params})
	if err != nil {
		return nil, fmt.Errorf("imessage: marshal request: %w", err)
	}

	stdout, err := a.execHelper(ctx, reqBody)
	if err != nil {
		// If the helper wrote a JSON error to stdout before exiting, use that.
		if len(stdout) > 0 {
			var resp helperResponse
			if json.Unmarshal(stdout, &resp) == nil && resp.Error != "" {
				return nil, fmt.Errorf("imessage: %s", resp.Error)
			}
		}
		return nil, fmt.Errorf("imessage: helper failed: %w", err)
	}

	var resp helperResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("imessage: parse helper response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("imessage: %s", resp.Error)
	}
	return &resp, nil
}

// execHelper launches the helper and returns its stdout. Uses `open` for .app
// bundles so macOS TCC attributes file access to the helper, not clawvisor.
func (a *IMessageAdapter) execHelper(ctx context.Context, reqBody []byte) ([]byte, error) {
	appDir := helperAppDir(a.helperPath)
	if appDir == "" {
		// Bare binary (dev/PATH fallback) — exec directly.
		cmd := exec.CommandContext(ctx, a.helperPath)
		cmd.Stdin = bytes.NewReader(reqBody)
		return cmd.Output()
	}

	// Write request to a temp file for stdin.
	stdinFile, err := os.CreateTemp("", "clawvisor-helper-req-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(stdinFile.Name())
	if _, err := stdinFile.Write(reqBody); err != nil {
		stdinFile.Close()
		return nil, err
	}
	stdinFile.Close()

	// Capture stdout via temp file.
	stdoutFile, err := os.CreateTemp("", "clawvisor-helper-resp-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(stdoutFile.Name())
	stdoutFile.Close()

	cmd := exec.CommandContext(ctx, "open", "-W", "-g", "-j", "-i", stdinFile.Name(), "-o", stdoutFile.Name(), appDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w — %s", err, truncate(string(out), 200))
	}

	return os.ReadFile(stdoutFile.Name())
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
