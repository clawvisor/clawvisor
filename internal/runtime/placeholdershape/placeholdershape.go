// Package placeholdershape is the single canonical home for detecting
// Clawvisor autovault placeholder substrings in arbitrary text.
//
// Inspector (parser/validator), script-session recognition, and any
// other site that needs "does this text contain a vaulted-credential
// placeholder?" all share this helper. Before this package existed,
// the regex was duplicated across packages with no enforced lockstep
// — a change in one spot could silently diverge from the rest.
//
// The package is a stdlib-only leaf so it can be imported from any
// llmproxy sub-package without cycles.
package placeholdershape

import "regexp"

// AutovaultRE matches an autovault placeholder substring anywhere in
// a blob of text. Anchored to the `autovault` literal so extraction
// (FindAllAutovault) returns just the placeholder, never the
// surrounding token-alphabet context. Detection (MatchString/Match)
// is unaffected — the pattern still matches whenever the placeholder
// appears in a larger string.
//
// Format: the literal "autovault" followed by at least one body char
// (alphanumeric, dot, underscore, colon, dash). The body is
// permissive so future placeholder formats (e.g.
// `autovault_v2_<service>_<id>`) still match. Underscore is in the
// body class so real placeholders like `autovault_<svc>_<id>` are
// matched as a single token.
//
// Previously this regex allowed `[A-Za-z0-9._:-]*` as an optional
// prefix before `autovault`, which let FindAllAutovault return
// strings like `xxxautovault_x` (extracted token glued to surrounding
// context). Detection paths didn't care; extraction paths (audit-row
// placeholder lists, autovault/swap's resolve(candidate)) silently
// corrupted. The anchored form fixes that without affecting any
// MatchString/Match call site.
var AutovaultRE = regexp.MustCompile(`autovault[A-Za-z0-9._:-]+`)

// ContainsAutovault reports whether raw carries an autovault
// placeholder substring. Byte-flavored variant for callers that
// already hold []byte (e.g. JSON envelopes).
func ContainsAutovault(raw []byte) bool {
	return AutovaultRE.Match(raw)
}

// ContainsAutovaultString is the string-flavored variant.
func ContainsAutovaultString(s string) bool {
	return AutovaultRE.MatchString(s)
}

// FindAllAutovault returns every placeholder substring in s. Used by
// the inspector's per-tool-use extraction (audit rows record which
// specific placeholders the call referenced).
func FindAllAutovault(s string) []string {
	return AutovaultRE.FindAllString(s, -1)
}
