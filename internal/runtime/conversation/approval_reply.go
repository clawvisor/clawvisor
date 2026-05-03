package conversation

import (
	"regexp"
	"strings"
)

var approvalReplyRE = regexp.MustCompile(`(?i)\b(approve|deny)\s+(cv-[a-z0-9]{12})\b`)
var bareApprovalRE = regexp.MustCompile(`(?i)^\s*(approve|deny)\s*$`)

// ParseApprovalReplyText extracts the most recent approval reply from a block
// of user-visible text. It scans non-empty lines from bottom to top and
// returns the first explicit approval marker it finds, allowing clients to
// wrap an approval with metadata or follow-up commentary.
func ParseApprovalReplyText(text string) (verb, id string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if match := approvalReplyRE.FindStringSubmatch(line); match != nil {
			return strings.ToLower(match[1]), strings.ToLower(match[2])
		}
		if match := bareApprovalRE.FindStringSubmatch(line); match != nil {
			return strings.ToLower(match[1]), ""
		}
	}
	return "", ""
}
