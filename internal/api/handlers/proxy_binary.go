package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ProxyBinaryHandler serves clawvisor-proxy binaries from an
// operator-configured directory. Public (no auth) — the binary is
// open-source and the operator decides who can reach the server. For
// multi-tenant cloud, leave BinaryDir empty to disable the endpoint
// and rely on GitHub releases.
//
// This is the dev-iteration path: rebuild proxy with `make build`
// inside third_party/proxy, set CLAWVISOR_PROXY_BINARY_DIR to that
// dist/, and `clawvisor proxy update-binary --from-server` picks it
// up. Avoids the GitHub-tag-and-release dance during development.
type ProxyBinaryHandler struct {
	binaryDir string
	logger    *slog.Logger
}

func NewProxyBinaryHandler(binaryDir string, logger *slog.Logger) *ProxyBinaryHandler {
	return &ProxyBinaryHandler{binaryDir: binaryDir, logger: logger}
}

// allowedPlatforms is the list of GOOS-GOARCH combinations the endpoint
// will serve. Anything else 400s. Keeps the path-traversal surface tiny.
var allowedPlatforms = map[string]bool{
	"darwin-arm64":  true,
	"darwin-amd64":  true,
	"linux-amd64":   true,
	"linux-arm64":   true,
	"windows-amd64": true,
}

// Download handles GET /api/proxy/download?platform=darwin-arm64.
// Looks for clawvisor-proxy-{platform} in BinaryDir, falls back to
// the unsuffixed clawvisor-proxy when the host happens to match the
// requested platform (the common dev case where you only built for
// your own machine).
func (h *ProxyBinaryHandler) Download(w http.ResponseWriter, r *http.Request) {
	if h.binaryDir == "" {
		writeError(w, http.StatusNotFound, "DOWNLOAD_DISABLED",
			"server is not configured to serve proxy binaries — set CLAWVISOR_PROXY_BINARY_DIR or use the GitHub-release path")
		return
	}

	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if platform == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"platform query parameter is required (e.g. darwin-arm64)")
		return
	}
	if !allowedPlatforms[platform] {
		writeError(w, http.StatusBadRequest, "INVALID_PLATFORM",
			"platform must be one of: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64, windows-amd64")
		return
	}

	path, err := h.resolveBinary(platform)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "BINARY_NOT_FOUND",
				fmt.Sprintf("no binary for platform %s in %s — build it first", platform, h.binaryDir))
			return
		}
		h.logger.Error("proxy binary lookup failed", "err", err, "platform", platform)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not resolve binary")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		h.logger.Error("proxy binary open failed", "err", err, "path", path)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not open binary")
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not stat binary")
		return
	}

	// Stream into a sha256 hasher first, then re-open to send. The two
	// reads are cheaper than buffering the whole binary in memory, and
	// the integrity header lets the CLI verify what it got matches the
	// announced digest.
	sum, err := fileSHA256(path)
	if err != nil {
		h.logger.Error("proxy binary hash failed", "err", err, "path", path)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not hash binary")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=clawvisor-proxy-%s", platform))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	w.Header().Set("X-Clawvisor-Proxy-Sha256", sum)
	w.Header().Set("X-Clawvisor-Proxy-Mtime", stat.ModTime().UTC().Format("2006-01-02T15:04:05Z"))

	if _, err := io.Copy(w, f); err != nil {
		h.logger.Warn("proxy binary stream interrupted", "err", err, "path", path)
	}
}

// resolveBinary picks the on-disk file for a platform. Tries the
// platform-suffixed name first, falls back to the unsuffixed binary
// (dev convenience: when you `make build` for your own machine, the
// file lands at dist/clawvisor-proxy with no suffix).
func (h *ProxyBinaryHandler) resolveBinary(platform string) (string, error) {
	suffixed := filepath.Join(h.binaryDir, "clawvisor-proxy-"+platform)
	if _, err := os.Stat(suffixed); err == nil {
		return suffixed, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	plain := filepath.Join(h.binaryDir, "clawvisor-proxy")
	if _, err := os.Stat(plain); err == nil {
		return plain, nil
	} else {
		return "", err
	}
}

func fileSHA256(path string) (string, error) {
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
