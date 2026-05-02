package policy

import (
	"encoding/base64"
	"regexp"
	"strings"
)

var base64LikePattern = regexp.MustCompile(`^[A-Za-z0-9+/=]{24,}$`)

func whyQualityIssues(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []string{"missing rationale"}
	}

	var issues []string
	if len(trimmed) < 12 {
		issues = append(issues, "rationale is too short to explain the intended use")
	}

	lowered := strings.ToLower(trimmed)
	genericPhrases := []string{
		"general use",
		"various tasks",
		"as needed",
		"miscellaneous",
		"do what is needed",
	}
	for _, phrase := range genericPhrases {
		if strings.Contains(lowered, phrase) {
			issues = append(issues, "rationale is too vague")
			break
		}
	}

	instructionPhrases := []string{
		"ignore previous",
		"ignore all previous",
		"system override",
		"disregard all",
		"priority instruction",
		"mark the overall task",
	}
	for _, phrase := range instructionPhrases {
		if strings.Contains(lowered, phrase) {
			issues = append(issues, "rationale contains instruction-like text instead of a task explanation")
			break
		}
	}

	compact := strings.ReplaceAll(trimmed, " ", "")
	if base64LikePattern.MatchString(compact) {
		if _, err := base64.StdEncoding.DecodeString(compact); err == nil {
			issues = append(issues, "rationale looks encoded rather than human-readable")
		}
	}

	return issues
}
