package main

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

//go:embed shim/clawvisor-proxy-shim.js
var nodeProxyShim []byte

// materializeNodeProxyShim writes the embedded shim to disk and returns its
// path. The file is content-checked so repeated runs don't rewrite it.
func materializeNodeProxyShim(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "clawvisor-proxy-shim.js")
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, nodeProxyShim) {
		return dst, nil
	}
	if err := os.WriteFile(dst, nodeProxyShim, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

// mergeNodeOptions appends addition to the existing NODE_OPTIONS value while
// preserving the user's current flags.
func mergeNodeOptions(existing, addition string) string {
	existing = strings.TrimSpace(existing)
	addition = strings.TrimSpace(addition)
	if existing == "" {
		return addition
	}
	if addition == "" {
		return existing
	}
	return existing + " " + addition
}
