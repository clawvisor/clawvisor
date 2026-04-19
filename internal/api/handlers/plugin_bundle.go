package handlers

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/pluginbundle"
)

// PluginBundleHandler serves the embedded OpenClaw plugin tarball so a
// Clawvisor instance can hand its own users a correctly-versioned plugin
// without depending on a separate registry. The tarball is produced at
// build time by `npm --prefix extensions/clawvisor run bundle` and
// embedded via `//go:embed` in internal/pluginbundle.
type PluginBundleHandler struct {
	logger *slog.Logger
}

func NewPluginBundleHandler(logger *slog.Logger) *PluginBundleHandler {
	return &PluginBundleHandler{logger: logger}
}

// ServeTarball handles GET /skill/openclaw-plugin.tgz (and the
// version-pinned /skill/openclaw-plugin-v{version}.tgz alias). Returns
// 503 PLUGIN_BUNDLE_UNAVAILABLE on dev builds that skipped the bundle
// step, so operators see a clear error rather than a confusing empty
// response.
func (h *PluginBundleHandler) ServeTarball(w http.ResponseWriter, r *http.Request) {
	if !pluginbundle.Available() {
		writeError(w, http.StatusServiceUnavailable, "PLUGIN_BUNDLE_UNAVAILABLE",
			"this Clawvisor build was compiled without the OpenClaw plugin tarball — run `make plugin-bundle` and rebuild")
		return
	}
	body, err := pluginbundle.Tarball()
	if err != nil {
		h.logger.Warn("plugin bundle read failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not read plugin bundle")
		return
	}
	// Short cache: plugin source changes at release cadence, and users
	// re-curl when they follow /skill/setup-openclaw — we don't want
	// intermediates pinning a stale version for hours.
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="openclaw-plugin.tgz"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Clawvisor-Plugin-Version", pluginbundle.Version())
	_, _ = w.Write(body)
}

// ServeSHA256 handles GET /skill/openclaw-plugin.sha256 in
// sha256sum-compatible plain-text format, so users can verify with a
// single pipeline: `curl -sL …/openclaw-plugin.tgz | sha256sum -c <(curl
// -sL …/openclaw-plugin.sha256)`.
func (h *PluginBundleHandler) ServeSHA256(w http.ResponseWriter, r *http.Request) {
	if !pluginbundle.Available() {
		writeError(w, http.StatusServiceUnavailable, "PLUGIN_BUNDLE_UNAVAILABLE",
			"plugin bundle not produced for this build")
		return
	}
	body, err := pluginbundle.SHA256()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not read plugin bundle hash")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}

// ServeVersion handles GET /skill/openclaw-plugin.version — plain-text
// version identifier, useful for scripts that want to check without
// parsing the tarball.
func (h *PluginBundleHandler) ServeVersion(w http.ResponseWriter, r *http.Request) {
	v := pluginbundle.Version()
	if v == "" {
		writeError(w, http.StatusServiceUnavailable, "PLUGIN_BUNDLE_UNAVAILABLE",
			"plugin bundle not produced for this build")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(strings.TrimSpace(v) + "\n"))
}
