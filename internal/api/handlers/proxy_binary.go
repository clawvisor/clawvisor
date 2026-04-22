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
// operator-configured directory.
//
// ┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
// ┃ DEV-ONLY, TEMPORARY — REMOVE WHEN CI PUBLISHES PROXY RELEASES.   ┃
// ┃ This endpoint exists solely so developers can iterate on the     ┃
// ┃ proxy against `make run` without cutting a GitHub release for    ┃
// ┃ every change. It is NOT the production distribution path.        ┃
// ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛
//
// Public (no auth) — the binary is open-source and the operator
// decides who can reach the server. For multi-tenant cloud, leave
// BinaryDir empty to disable the endpoint entirely and rely on the
// GitHub-release path the CLI already falls back to.
//
// Dev workflow:
//   cd third_party/proxy && make build
//   export CLAWVISOR_PROXY_BINARY_DIR=$(pwd)/third_party/proxy/dist
//   make run
//   # on a client machine:
//   clawvisor-local proxy update-binary --from-server --server-url http://127.0.0.1:25297
//
// Replacement plan: once CI produces platform-tagged release assets
// (clawvisor-proxy-darwin-arm64, -linux-amd64, etc.) on every merge,
// this endpoint + its CLI flag are redundant — `update-binary` will
// pull from releases by default and this file can be deleted.
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
// platform-suffixed name first, falls back to the unsuffixed binary.
//
// The unsuffixed fallback is intentional for the dev case: `make
// build` inside third_party/proxy produces `dist/clawvisor-proxy`
// (no platform suffix, because the dev is building for their own
// machine and doesn't cross-compile). A client requesting
// darwin-arm64 and hitting a server whose dist/ only has that single
// build will get served correctly — the dev's host IS the requested
// platform.
//
// KNOWN LIMITATION (acceptable in dev, unacceptable in prod): if a
// server has a single unsuffixed binary for platform X and a client
// requests platform Y, we serve the X binary anyway. That's a bug
// for any multi-platform dev setup, but this endpoint is dev-only
// (see the handler doc) and a real CI pipeline produces per-platform
// files that skip this fallback. The planned replacement drops this
// fallback entirely.
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
