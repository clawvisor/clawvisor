package llmproxy

import (
	"regexp"
	"strings"
)

// NoticeKind classifies a Clawvisor control-plane notice the proxy
// substitutes into a user-role transcript turn. Kinds are descriptive
// labels for proxy-state categories (task approval outcomes, scope
// decisions, …); the system-prompt preamble teaches the envelope
// semantics so the model can recognize the channel without needing to
// know every kind. Concrete kinds are defined alongside the call sites
// that emit them (see inline_task_rewrite.go for task-* kinds).
//
// The new envelope replaces the role-mixed `[Clawvisor: …]` prefix
// used for these injections previously; the assistant-side, human-
// visible `[Clawvisor]` lines (routing, auto-approval, observe mode)
// are NOT routed through Render — they're plain backticked text the
// human reading the chat sees, not structured payloads the model
// parses.
type NoticeKind string

// noticeTagOpenPrefix / noticeTagClose anchor the wire format. Callers
// that scan for proxy-emitted notices (inbound scrubbers, defense-in-
// depth filters) match on these constants rather than the assembled
// element so the literal stays in one place.
const (
	noticeTagOpenPrefix = "<clawvisor-notice"
	noticeTagClose      = "</clawvisor-notice>"
)

// noticeKindShape constrains kind to a small, safe alphabet so the
// rendered attribute can never escape its quotes. The XML escaper below
// also handles quotes defensively, but rejecting non-conforming kinds
// up front keeps the wire format stable and obviously safe.
var noticeKindShape = regexp.MustCompile(`^[a-z0-9-]+$`)

// Render returns a single <clawvisor-notice kind="..."> element wrapping
// body. Both the kind attribute and body are XML-escaped so model-
// authored or operator-authored substrings containing `<`, `>`, `&`,
// or quotes cannot corrupt the envelope or smuggle a forged closing
// tag. An invalid (or empty) kind degrades to the safe fallback
// kind="notice" — Render never panics, never returns the empty string
// when given a non-empty body, and never produces malformed XML.
func Render(kind NoticeKind, body string) string {
	k := strings.TrimSpace(string(kind))
	if k == "" || !noticeKindShape.MatchString(k) {
		k = "notice"
	}
	var b strings.Builder
	b.Grow(len(noticeTagOpenPrefix) + len(k) + len(body) + len(noticeTagClose) + 16)
	b.WriteString(noticeTagOpenPrefix)
	b.WriteString(` kind="`)
	b.WriteString(escapeXMLAttr(k))
	b.WriteString(`">`)
	b.WriteString(escapeXMLText(body))
	b.WriteString(noticeTagClose)
	return b.String()
}

func escapeXMLAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

func escapeXMLText(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}
