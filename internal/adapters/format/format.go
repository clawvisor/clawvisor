// Package format provides helpers for building safe, sanitized SemanticResults.
package format

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	MaxBodyLen    = 200_000
	MaxSnippetLen = 300
	MaxFieldLen   = 500
	MaxArrayItems = 200
	MaxDataBytes  = 100 * 1024
)

// SanitizeText strips HTML, removes dangerous Unicode, and truncates to maxLen runes.
// If maxLen <= 0, only sanitization is applied (no truncation).
func SanitizeText(s string, maxLen int) string {
	s = stripHTML(s)
	s = removeDangerousUnicode(s)
	s = strings.TrimSpace(s)
	if maxLen > 0 && utf8.RuneCountInString(s) > maxLen {
		runes := []rune(s)
		s = string(runes[:maxLen]) + " [truncated]"
	}
	return s
}

// SanitizeHeader removes dangerous Unicode and truncates, but does NOT strip
// HTML. Use this for email header fields (From, To, Cc, Reply-To) where
// angle-bracket addresses like <user@example.com> must be preserved.
func SanitizeHeader(s string, maxLen int) string {
	s = removeDangerousUnicode(s)
	s = strings.TrimSpace(s)
	if maxLen > 0 && utf8.RuneCountInString(s) > maxLen {
		runes := []rune(s)
		s = string(runes[:maxLen]) + " [truncated]"
	}
	return s
}

// Summary builds a one-line summary using fmt.Sprintf-style formatting.
func Summary(template string, args ...any) string {
	if len(args) == 0 {
		return template
	}
	return fmt.Sprintf(template, args...)
}

// TruncateSlice returns at most max items from the slice.
func TruncateSlice[T any](items []T, max int) []T {
	if len(items) <= max {
		return items
	}
	return items[:max]
}

// StripSecrets removes keys that look like credentials from a map.
// Operates on a shallow copy — does not modify the original.
func StripSecrets(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if isSecretKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// ── HTML stripping ────────────────────────────────────────────────────────────

func stripHTML(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		// If we can't parse, fall back to simple tag removal
		return stripHTMLFallback(s)
	}
	var buf strings.Builder
	extractText(doc, &buf)
	return buf.String()
}

func extractText(n *html.Node, buf *strings.Builder) {
	if n.Type == html.TextNode {
		buf.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		extractText(c, buf)
	}
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

func stripHTMLFallback(s string) string {
	return htmlTagRe.ReplaceAllString(s, "")
}

// ── Dangerous Unicode removal ─────────────────────────────────────────────────

func removeDangerousUnicode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isDangerous(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isDangerous(r rune) bool {
	// Zero-width characters
	if r == '\u200B' || r == '\u200C' || r == '\u200D' || r == '\uFEFF' {
		return true
	}
	// BiDi override characters
	if r >= '\u200E' && r <= '\u200F' {
		return true
	}
	if r >= '\u202A' && r <= '\u202E' {
		return true
	}
	if r >= '\u2066' && r <= '\u2069' {
		return true
	}
	// Unicode tag block (used to hide payloads)
	if r >= '\U000E0000' && r <= '\U000E007F' {
		return true
	}
	// Variation selectors
	if r >= '\uFE00' && r <= '\uFE0F' {
		return true
	}
	if r >= '\U000E0100' && r <= '\U000E01EF' {
		return true
	}
	// Non-printable control chars (keep common ones like \n, \t)
	if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
		return true
	}
	return false
}

// ── Secret key detection ──────────────────────────────────────────────────────

var secretKeyPatterns = []string{
	"token", "secret", "password", "passwd", "credential", "auth",
	"api_key", "apikey", "access_key", "private_key", "bearer",
}

func isSecretKey(k string) bool {
	lower := strings.ToLower(k)
	for _, pattern := range secretKeyPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
