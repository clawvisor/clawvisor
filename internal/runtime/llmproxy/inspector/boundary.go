package inspector

import (
	"strings"
)

// BoundaryCheck validates the inspector's verdict against ground-truth
// data the validator's output must NOT be trusted to provide alone:
//
//   - The inferred host must match (or be in) the placeholder's
//     bound-service host allowlist.
//   - Verdicts marked ambiguous fail closed regardless of any other field.
//
// Returns ok=true when the verdict is safe to act on. The reason string
// is for audit; it's empty on success.
//
// allowedHosts is the set of canonical hosts the placeholder's bound
// service is authorized to forward to. The set comes from the catalog
// at policy-version-snapshot time (callers pin to a versioned snapshot
// to ensure a poisoned catalog edit doesn't widen scope mid-flight).
func BoundaryCheck(v Verdict, allowedHosts []string) (ok bool, reason string) {
	if v.Ambiguous {
		return false, "verdict ambiguous"
	}
	if !v.IsAPICall {
		return false, "verdict says not an API call"
	}
	if v.Host == "" {
		return false, "verdict missing target host"
	}
	if len(allowedHosts) == 0 {
		return false, "no allowed hosts for bound service"
	}
	if !hostInAllowlist(v.Host, allowedHosts) {
		return false, "verdict host not in bound-service allowlist"
	}
	return true, ""
}

// hostInAllowlist returns true when host exactly matches one of the
// allowed entries, or — when an allowed entry begins with the literal
// "*." — when host's domain suffix matches. Pure substring matching is
// deliberately rejected: `api.github.com.attacker.com` must not match
// `api.github.com`.
func hostInAllowlist(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:] // ".github.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}
