package conversation

import (
	"regexp"
	"strings"
)

var approvalReplyRE = regexp.MustCompile(`(?i)^\s*(approve|deny|yes|y|no|n|task)\s+(cv-(?:[a-z0-9]{12}|[a-z0-9]{26}))\s*$`)
var bareApprovalRE = regexp.MustCompile(`(?i)^\s*(approve|deny|yes|y|no|n|task)\s*$`)

// ApprovalIDMarker prefixes the parseable footer that the proxy appends to
// every approval prompt it renders. Format: "[clawvisor:approval=<id>]".
// Lives in this package (rather than llmproxy) because both the request-body
// parsers and the prompt renderers need to agree on the literal.
const ApprovalIDMarker = "[clawvisor:approval="

var approvalMarkerRE = regexp.MustCompile(`\[clawvisor:approval=(cv-[a-z0-9]+)\]`)

// FindLatestApprovalIDMarker returns the approval ID from the rightmost
// [clawvisor:approval=cv-...] marker in text, or "" if none is present.
// The proxy embeds this footer in approval prompts so that subsequent turns
// can route a bare "y"/"n" reply to the specific hold the user is looking at,
// even when several pending approvals coexist in the same transcript.
func FindLatestApprovalIDMarker(text string) string {
	if text == "" {
		return ""
	}
	matches := approvalMarkerRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return ""
	}
	return strings.ToLower(matches[len(matches)-1][1])
}

// ParseApprovalReplyText extracts the most recent approval reply from a block
// of user-visible text. User-facing yes/no replies are normalized to the
// canonical approve/deny verbs used by the release pipeline. It scans non-empty
// lines from bottom to top and returns the first explicit approval marker it
// finds, allowing clients to wrap an approval with metadata or follow-up
// commentary.
func ParseApprovalReplyText(text string) (verb, id string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if match := approvalReplyRE.FindStringSubmatch(line); match != nil {
			return normalizeApprovalReplyVerb(match[1]), strings.ToLower(match[2])
		}
		if match := bareApprovalRE.FindStringSubmatch(line); match != nil {
			return normalizeApprovalReplyVerb(match[1]), ""
		}
	}
	return "", ""
}

func normalizeApprovalReplyVerb(verb string) string {
	switch strings.ToLower(verb) {
	case "y", "yes":
		return "approve"
	case "n", "no":
		return "deny"
	default:
		return strings.ToLower(verb)
	}
}
