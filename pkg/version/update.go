package version

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

// Apply downloads the target version, verifies its checksum, and atomically
// replaces the current binary. It returns the old and new version strings.
// The caller is responsible for any restart logic (daemon restart, process re-exec, etc.).
func Apply(targetVersion string) (oldVersion, newVersion string, err error) {
	oldVersion = Version
	newVersion = strings.TrimPrefix(targetVersion, "v")
	tag := "v" + newVersion

	assetName := fmt.Sprintf("clawvisor-%s-%s", runtime.GOOS, runtime.GOARCH)
	baseURL := fmt.Sprintf("https://github.com/clawvisor/clawvisor/releases/download/%s", tag)
	binaryURL := baseURL + "/" + assetName
	checksumsURL := baseURL + "/checksums.txt"

	client := &http.Client{Timeout: 2 * time.Minute}

	// Download checksums.
	checksumsBody, err := httpGet(client, checksumsURL)
	if err != nil {
		return oldVersion, newVersion, fmt.Errorf("downloading checksums: %w", err)
	}
	expectedHash, err := findChecksum(string(checksumsBody), assetName)
	if err != nil {
		return oldVersion, newVersion, err
	}

	// Download binary to temp file.
	tmpDir, err := os.MkdirTemp("", "clawvisor-update-*")
	if err != nil {
		return oldVersion, newVersion, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpBinary := filepath.Join(tmpDir, assetName)
	if err := httpDownload(client, binaryURL, tmpBinary); err != nil {
		return oldVersion, newVersion, fmt.Errorf("downloading binary: %w", err)
	}

	// Verify checksum.
	actualHash, err := sha256File(tmpBinary)
	if err != nil {
		return oldVersion, newVersion, fmt.Errorf("computing checksum: %w", err)
	}
	if actualHash != expectedHash {
		return oldVersion, newVersion, fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	// Make executable and atomic-replace the current binary.
	if err := os.Chmod(tmpBinary, 0755); err != nil {
		return oldVersion, newVersion, fmt.Errorf("chmod: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return oldVersion, newVersion, fmt.Errorf("resolving current binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return oldVersion, newVersion, fmt.Errorf("resolving symlinks: %w", err)
	}

	if err := os.Rename(tmpBinary, exe); err != nil {
		return oldVersion, newVersion, fmt.Errorf("replacing binary: %w", err)
	}

	return oldVersion, newVersion, nil
}

func httpGet(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func httpDownload(client *http.Client, url, dest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func findChecksum(checksums, assetName string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == assetName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s in checksums.txt", assetName)
}
