package adapters

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Agent-supplied identifiers end up inside upstream URL paths. A value that
// carries "/", "?", "#" or a "." / ".." segment can re-point an authorized
// action at a different upstream endpoint while the gateway still records the
// authorized one. The helpers below are the shared defense: adapters escape
// values with PathSegment / PathSegments, and GuardPathTraversal refuses to
// send any request whose path still contains a dot segment.

// PathSegment escapes v for use as exactly one URL path segment. "/", "?" and
// "#" are percent-encoded. Empty values are rejected, as are values with a
// "." or ".." component even between encoded slashes, since some upstreams
// decode "%2F" before resolving dot segments.
func PathSegment(v string) (string, error) {
	if v == "" {
		return "", checkSegment(v)
	}
	if hasDotComponent(v) {
		return "", fmt.Errorf("path value must not contain \".\" or \"..\" segments")
	}
	return url.PathEscape(v), nil
}

// PathSegments escapes v for use as one or more URL path segments, keeping
// "/" as a separator for multi-segment resource names like "people/123".
// Every segment is escaped individually and must be non-empty and not a dot
// segment.
func PathSegments(v string) (string, error) {
	parts := strings.Split(v, "/")
	for i, part := range parts {
		if err := checkSegment(part); err != nil {
			return "", err
		}
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/"), nil
}

func checkSegment(s string) error {
	switch s {
	case "":
		return fmt.Errorf("path value must not be empty or contain empty segments")
	case ".", "..":
		return fmt.Errorf("path value must not contain %q segments", s)
	}
	return nil
}

// HasDotSegment reports whether an escaped URL path contains a "." or ".."
// segment, including percent-encoded forms such as "%2e%2E" and segments
// hidden behind an encoded slash such as "..%2F..".
func HasDotSegment(escapedPath string) bool {
	for _, seg := range strings.Split(escapedPath, "/") {
		if decoded, err := url.PathUnescape(seg); err == nil {
			seg = decoded
		}
		if hasDotComponent(seg) {
			return true
		}
	}
	return false
}

// hasDotComponent reports whether any "/"-separated component of s is "." or
// "..".
func hasDotComponent(s string) bool {
	for _, part := range strings.Split(s, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

// GuardPathTraversal returns a copy of c whose transport refuses requests with
// dot segments in the path. Upstreams resolve those segments, so a request
// that still carries one would reach an endpoint other than the one the
// adapter built it for.
func GuardPathTraversal(c *http.Client) *http.Client {
	guarded := *c
	guarded.Transport = &pathGuardTransport{base: c.Transport}
	return &guarded
}

type pathGuardTransport struct {
	base http.RoundTripper
}

func (t *pathGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if HasDotSegment(req.URL.EscapedPath()) {
		// RoundTrip must close the body even when it fails.
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("refusing request with dot segment in path %q", req.URL.EscapedPath())
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
