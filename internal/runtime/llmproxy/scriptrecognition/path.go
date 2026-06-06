package scriptrecognition

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// NormalizeScriptSessionPathPrefix canonicalizes a candidate path prefix
// from the mint request. Rules:
//
//   - must be a non-empty absolute path (leading "/", but not just "/")
//   - no scheme, no authority, no query, no fragment
//   - no "..", "%2e%2e", or other encoded path-traversal
//   - cleaned via path.Clean to fold "//", trailing slashes, and "."
//
// Returns the cleaned form on success. The cleaned form intentionally
// has no trailing slash; ScriptSessionPathPrefixMatch handles trailing
// slash semantics, so requiring callers to submit both "/x" and "/x/"
// would be redundant.
func NormalizeScriptSessionPathPrefix(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path prefix is empty")
	}
	// Reject scheme + authority shapes outright. url.Parse accepts
	// inputs like "https://attacker.example/foo" with Scheme/Host
	// populated, which a naive HasPrefix check would not catch.
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("path prefix is not parseable as a URL path: " + err.Error())
	}
	if u.Scheme != "" || u.Host != "" {
		return "", errors.New("path prefix must not include scheme or host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("path prefix must not include query or fragment")
	}
	// Reject encoded path-traversal before url.Parse decodes it. We
	// compare against the raw input so "%2e%2e" never slips past as ".."
	// after a later decode step on the resolver side.
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2e%2e") || strings.Contains(lower, "%2e%2f") || strings.Contains(lower, "%2f%2e") {
		return "", errors.New("path prefix must not include percent-encoded traversal")
	}
	p := u.Path
	if p == "" {
		p = raw
	}
	if !strings.HasPrefix(p, "/") {
		return "", errors.New("path prefix must start with /")
	}
	if strings.Contains(p, "..") {
		return "", errors.New("path prefix must not include ..")
	}
	cleaned := path.Clean(p)
	if cleaned == "/" || cleaned == "." {
		return "", errors.New("path prefix must be service-specific (not / or empty)")
	}
	if !strings.HasPrefix(cleaned, "/") {
		// Defensive: path.Clean preserves leading "/", but if a future
		// stdlib change altered that we'd silently widen the scope.
		return "", errors.New("path prefix must start with /")
	}
	return cleaned, nil
}

// ScriptSessionPathPrefixMatch reports whether requestPath is within
// the approved prefix. Match rule:
//
//   - exact match: requestPath == prefix
//   - subpath match: strings.HasPrefix(requestPath, prefix + "/")
//
// requestPath should be the upstream path with query/fragment stripped.
// A leading slash is required.
//
// This precision matters: a naive strings.HasPrefix would accept
// /gmail/v1/users/me/messages-evil under prefix
// /gmail/v1/users/me/messages, which is a privilege-escalation across
// an adjacent endpoint sharing the same path stem.
//
// Dot-segment traversal is rejected outright. Without this guard a
// request such as /gmail/v1/users/me/messages/../profile would pass
// HasPrefix for prefix /gmail/v1/users/me/messages — and most upstream
// servers (and intermediate proxies / load balancers) collapse `..`
// before routing, delivering the request to /gmail/v1/users/me/profile,
// outside the session's approved scope. We refuse both literal `..` and
// the common percent-encoded forms (case-insensitive) so a normalizer
// run later in the request stack can't "rescue" a forbidden traversal.
func ScriptSessionPathPrefixMatch(prefix, requestPath string) bool {
	if prefix == "" || requestPath == "" {
		return false
	}
	if pathHasTraversal(requestPath) {
		return false
	}
	if requestPath == prefix {
		return true
	}
	return strings.HasPrefix(requestPath, prefix+"/")
}

// pathHasTraversal returns true when p contains a traversal-shaped
// substring in any form a downstream URL normalizer might collapse:
// a literal `..`, or one of the common percent-encoded equivalents
// (`%2e%2e`, `%2e.`, `.%2e`, `%2f%2e`, `%2e%2f`).
//
// The check is a pure substring scan, not a path.Clean comparison.
// That deliberately rejects ANY `..` substring, including a
// legitimate path segment like `/foo..bar/baz` (uncommon but valid
// per RFC 3986). v1 accepts the over-rejection: real upstream API
// surfaces don't put `..` inside segment names, and the cost of being
// strict is missing out on a handful of pathological-but-legal paths
// in exchange for the certainty that no traversal-shaped substring
// escapes the boundary. Revisit if a real upstream requires `..` in
// a segment, in which case switch to a path.Clean comparison and
// only flag paths whose cleaned form drops intermediate components.
func pathHasTraversal(p string) bool {
	if strings.Contains(p, "..") {
		return true
	}
	lower := strings.ToLower(p)
	// Percent-encoded forms: %2e is `.`, %2f is `/`. Any pair that
	// could re-emerge as `..` or `/.` after decoding is suspect.
	if strings.Contains(lower, "%2e%2e") ||
		strings.Contains(lower, "%2e.") ||
		strings.Contains(lower, ".%2e") ||
		strings.Contains(lower, "%2f%2e") ||
		strings.Contains(lower, "%2e%2f") {
		return true
	}
	return false
}
