package handlers

import (
	"net/http"
	"strings"
)

// ConfigHandler serves unauthenticated public configuration used by the web UI.
type ConfigHandler struct {
	authMode           string
	proxyLitePublicURL string
}

func NewConfigHandler(authMode string, proxyLitePublicURL string) *ConfigHandler {
	return &ConfigHandler{
		authMode:           authMode,
		proxyLitePublicURL: strings.TrimRight(strings.TrimSpace(proxyLitePublicURL), "/"),
	}
}

// Public returns public configuration (no auth required).
func (h *ConfigHandler) Public(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_mode":             h.authMode,
		"proxy_lite_public_url": h.proxyLitePublicURL,
	})
}
