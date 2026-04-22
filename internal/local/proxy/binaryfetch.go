package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DownloadBinaryFromServer fetches the platform-appropriate proxy
// binary from a Clawvisor server's /api/proxy/download endpoint,
// verifies the announced sha256 from X-Clawvisor-Proxy-Sha256, and
// atomically writes the result to dst with 0755 permissions.
//
// This is the path the dashboard one-click connect flow and the
// `clawvisor-local proxy update-binary --from-server` CLI both go
// through — kept here (not in cmd/) so the daemon's HTTP handler
// can call it without cross-package dependencies.
func DownloadBinaryFromServer(serverURL, dst string) error {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	u := strings.TrimRight(serverURL, "/") + "/api/proxy/download?platform=" + platform

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("server %d: %s", resp.StatusCode, string(raw))
	}
	wantSum := resp.Header.Get("X-Clawvisor-Proxy-Sha256")

	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("stream to disk: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if wantSum != "" && !strings.EqualFold(gotSum, wantSum) {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: server announced %s, got %s", wantSum, gotSum)
	}
	return os.Rename(tmp, dst)
}

// DefaultBinaryPath returns the conventional install location used by
// the daemon-managed proxy lifecycle. Kept in one place so the daemon
// endpoint, CLI, and dashboard all agree on where binaries live.
func DefaultBinaryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".clawvisor", "proxy", "bin", "clawvisor-proxy")
}

