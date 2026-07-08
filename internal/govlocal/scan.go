package govlocal

import (
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// patternMatchSafe applies a content policy's pattern to content.
//
// Ported verbatim (behavior-identical) from cloud
// internal/governance/callbacks.go's patternMatchSafe — the OSS and cloud
// content scanners MUST match byte-for-byte so the Terraform provider's
// content_policy resource behaves identically across tiers (PRD §8). The
// only structural change is the policy type (store.InstanceContentPolicy
// vs cloud's OrgContentPolicy); the matching semantics are unchanged.
//
// ReDoS guards (mandatory — do not relax): regex patterns > 256 chars
// never match; compile errors never match; content is truncated to 65536
// bytes before MatchString. keyword patterns are a case-insensitive
// substring match.
func patternMatchSafe(p *store.InstanceContentPolicy, content string) bool {
	if p.PatternKind == "keyword" {
		return strings.Contains(strings.ToLower(content), strings.ToLower(p.Pattern))
	}
	if p.PatternKind == "regex" {
		// Cap pattern length + content length. ReDoS protection: if a
		// regex compiles, we test against a length-capped content.
		if len(p.Pattern) > 256 {
			return false
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return false
		}
		// Length-cap the input the regex sees to bound worst-case time.
		const maxScan = 65536
		if len(content) > maxScan {
			content = content[:maxScan]
		}
		return re.MatchString(content)
	}
	return false
}
