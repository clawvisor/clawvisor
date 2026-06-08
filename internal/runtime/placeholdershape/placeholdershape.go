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
// a blob of text. The pattern allows word-character context on either
// side because real placeholders appear inside Authorization headers,
// shell variable assignments, JSON values, etc. — never as
// stand-alone tokens.
//
// Format: optional word-character lead-in, the literal "autovault",
// then at least one body char (alphanumeric, dot, underscore, colon,
// dash). The body is intentionally permissive so future placeholder
// formats (e.g. `autovault_v2_<service>_<id>`) still match.
var AutovaultRE = regexp.MustCompile(`[A-Za-z0-9._:-]*autovault[A-Za-z0-9._:-]+`)

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
