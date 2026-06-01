package autovault

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

// sanitizePlaceholderPrefix folds the lowercased service id to the
// [a-z0-9_] allowlist — everything outside becomes `_`. This must stay
// a strict subset of the inspector's placeholder character class
// (inspector/inspector.go shadowPlaceholderExtractRE,
// parser/parser.go autovaultPlaceholderRE) so any placeholder we
// generate round-trips through detection. A denylist of known
// separators ("." ":" "-" "/") had let chars like "@" survive into
// stored placeholders that the inspector then truncated mid-string.
func sanitizePlaceholderPrefix(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b[i] = c
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

func PlaceholderPrefix(service string) string {
	safe := sanitizePlaceholderPrefix(strings.ToLower(strings.TrimSpace(service)))
	if safe == "" {
		safe = "unknown"
	}
	return ShadowMarker + "_" + safe + "_"
}

const ShadowMarker = "autovault"

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
	return strings.Contains(strings.ToLower(v), ShadowMarker)
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
