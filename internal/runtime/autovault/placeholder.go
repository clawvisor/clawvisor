package autovault

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

var placeholderPrefixReplacer = strings.NewReplacer(".", "_", ":", "_", "-", "_", "/", "_")

func PlaceholderPrefix(service string) string {
	safe := placeholderPrefixReplacer.Replace(strings.ToLower(strings.TrimSpace(service)))
	if safe == "" {
		safe = "unknown"
	}
	return ShadowMarker + "_" + safe + "_"
}

const ShadowMarker = "autovault"
const legacyShadowMarker = "clawvisor"

func GeneratePlaceholder(prefix string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	suffix := base64.RawURLEncoding.EncodeToString(raw)
	if !LooksLikeShadow(prefix) {
		prefix = prefix + ShadowMarker
	}
	return prefix + suffix, nil
}

func LooksLikeShadow(v string) bool {
	lower := strings.ToLower(v)
	return strings.Contains(lower, ShadowMarker) || strings.Contains(lower, legacyShadowMarker)
}

func HeaderMaybeContainsShadow(v string) bool {
	if LooksLikeShadow(v) {
		return true
	}
	scheme, rest, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil {
		return false
	}
	return LooksLikeShadow(string(raw))
}
