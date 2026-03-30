package main

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

	"github.com/spf13/cobra"

	"github.com/clawvisor/clawvisor/internal/daemon"
	"github.com/clawvisor/clawvisor/pkg/version"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update Clawvisor to the latest release",
	RunE:  runUpdate,
}

func init() {
	updateCmd.Flags().String("version", "", "Update to a specific version instead of latest")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	pinned, _ := cmd.Flags().GetString("version")

	// Determine target version.
	var targetVersion, tag string
	if pinned != "" {
		targetVersion = strings.TrimPrefix(pinned, "v")
		tag = "v" + targetVersion
	} else {
		fmt.Println("  Checking for updates...")
		info := version.Check()
		if info.Latest == "" {
			return fmt.Errorf("could not determine latest version")
		}
		if !info.UpdateAvail {
			fmt.Printf("  Already up to date (v%s).\n", info.Current)
			return nil
		}
		targetVersion = info.Latest
		tag = "v" + targetVersion
	}

	current := version.GetCurrent()
	fmt.Printf("  Updating v%s → v%s\n", current, targetVersion)

	// Build asset name and download URL.
	assetName := fmt.Sprintf("clawvisor-%s-%s", runtime.GOOS, runtime.GOARCH)
	baseURL := fmt.Sprintf("https://github.com/clawvisor/clawvisor/releases/download/%s", tag)
	binaryURL := baseURL + "/" + assetName
	checksumsURL := baseURL + "/checksums.txt"

	client := &http.Client{Timeout: 2 * time.Minute}

	// Download checksums.
	fmt.Println("  Downloading checksums...")
	checksumsBody, err := httpGet(client, checksumsURL)
	if err != nil {
		return fmt.Errorf("downloading checksums: %w", err)
	}
	expectedHash, err := findChecksum(string(checksumsBody), assetName)
	if err != nil {
		return err
	}

	// Download binary to temp file.
	fmt.Println("  Downloading binary...")
	tmpDir, err := os.MkdirTemp("", "clawvisor-update-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpBinary := filepath.Join(tmpDir, assetName)
	if err := httpDownload(client, binaryURL, tmpBinary); err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}

	// Verify checksum.
	fmt.Println("  Verifying checksum...")
	actualHash, err := sha256File(tmpBinary)
	if err != nil {
		return fmt.Errorf("computing checksum: %w", err)
	}
	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch:\n  expected: %s\n  got:      %s", expectedHash, actualHash)
	}

	// Make executable and atomic-replace the current binary.
	if err := os.Chmod(tmpBinary, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving current binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	if err := os.Rename(tmpBinary, exe); err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}

	fmt.Printf("  Updated successfully: v%s → v%s\n", current, targetVersion)

	// Check if daemon is running and remind user to restart.
	if s, err := daemon.CheckStatus(); err == nil && s.Running {
		fmt.Println("  Daemon is running — restart it with: clawvisor restart")
	}

	return nil
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
		// Format: "<hash>  <filename>" or "<hash> <filename>"
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == assetName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s in checksums.txt", assetName)
}
