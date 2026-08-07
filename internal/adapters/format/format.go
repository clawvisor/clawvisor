// Package format provides helpers for building safe, sanitized SemanticResults.
package format

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
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

	// MaxDownloadBytes is the ceiling on binary file downloads. It is
	// deliberately separate from MaxBodyLen: that constant is a rune count
	// used to truncate human-readable text (mail bodies, event descriptions),
	// and downloads only ever borrowed it as a byte count.
	//
	// A download this large is only usable when the caller streams the
	// response to disk (e.g. curl > out.json) rather than reading it into an
	// LLM context — 10 MB of content is ~13.3 MB of base64.
	MaxDownloadBytes = 10 * 1024 * 1024

	// DefaultDownloadBytes is the cap applied when the caller does not ask
	// for a larger one. It is sized to survive being read into a model
	// context; callers that stream to disk opt in to MaxDownloadBytes
	// explicitly via a max_bytes parameter.
	DefaultDownloadBytes = 1024 * 1024
)

// ResolveMaxBytes turns a caller-supplied max_bytes parameter into a download
// ceiling. Shared by every adapter that returns file content so the contract —
// the default, the ceiling, and the validation — cannot drift between them.
//
// Absent means DefaultDownloadBytes, which is sized to survive being read into
// a model context. Callers that save the response to a file opt in to more, up
// to MaxDownloadBytes.
func ResolveMaxBytes(v any) (int64, error) {
	if v == nil {
		return DefaultDownloadBytes, nil
	}
	var n int64
	switch x := v.(type) {
	case float64: // JSON numbers decode as float64
		// Reject fractional values rather than truncating them: 0.5 would
		// silently become 0 and then fail as "must be positive", and 1.9 would
		// become 1, producing a limit the caller never asked for.
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("max_bytes must be a whole number, got %v", x)
		}
		// Range-check in float space. Converting an out-of-range float to
		// int64 is implementation-defined in Go, so a value like 1e19 would
		// otherwise reach the checks below as an arbitrary number that only
		// happens to be negative on common platforms.
		if x < 0 || x > float64(MaxDownloadBytes) {
			return 0, fmt.Errorf("max_bytes must be between 1 and %d, got %v", MaxDownloadBytes, x)
		}
		n = int64(x)
	case int:
		n = int64(x)
	case int64:
		n = x
	case json.Number:
		parsed, err := x.Int64()
		if err != nil {
			return 0, fmt.Errorf("max_bytes must be a whole number, got %v", x)
		}
		n = parsed
	default:
		return 0, fmt.Errorf("max_bytes must be a number")
	}
	if n <= 0 {
		return 0, fmt.Errorf("max_bytes must be positive")
	}
	if n > MaxDownloadBytes {
		return 0, fmt.Errorf("max_bytes may not exceed %d", MaxDownloadBytes)
	}
	return n, nil
}

// OverflowMessage explains that content exceeded a limit, and only suggests
// raising max_bytes when there is headroom left. At the ceiling the advice
// would name the very value the caller already has.
func OverflowMessage(limit int64) string {
	if limit >= MaxDownloadBytes {
		return fmt.Sprintf("exceeds the maximum supported size of %d bytes", MaxDownloadBytes)
	}
	return fmt.Sprintf("exceeds the %d byte limit; raise max_bytes (up to %d)", limit, MaxDownloadBytes)
}

// ReadBounded reads at most limit bytes and reports whether the source had
// more, so callers can refuse an oversized payload rather than returning a
// truncated one. Reading limit+1 is what makes the overflow detectable.
func ReadBounded(r io.Reader, limit int64) (body []byte, overflow bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(body)) > limit {
		return body[:limit], true, err
	}
	return body, false, err
}

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

// Truncate returns a string truncated to max characters with "..." appended if truncated.
func Truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) > max {
		runes := []rune(s)
		return string(runes[:max]) + "..."
	}
	return s
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
