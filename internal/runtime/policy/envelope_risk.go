package policy

import (
	"fmt"
	"net/http"
	"strings"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
)

func AssessTaskEnvelope(purpose string, env runtimetasks.Envelope) *taskrisk.RiskAssessment {
	level := "low"
	var factors []string
	var conflicts []taskrisk.ConflictDetail

	purposeIssues := whyQualityIssues(purpose)
	for _, issue := range purposeIssues {
		conflicts = append(conflicts, taskrisk.ConflictDetail{
			Field:       "purpose",
			Description: issue,
			Severity:    "warning",
		})
	}
	if len(purposeIssues) > 0 {
		level = maxRiskLevel(level, "medium")
	}

	if env.ExpectedUse != "" {
		useIssues := whyQualityIssues(env.ExpectedUse)
		for _, issue := range useIssues {
			conflicts = append(conflicts, taskrisk.ConflictDetail{
				Field:       "expected_use",
				Description: issue,
				Severity:    "warning",
			})
		}
		if len(useIssues) > 0 {
			level = maxRiskLevel(level, "medium")
		}
	}

	switch env.IntentVerificationMode {
	case "lenient":
		factors = append(factors, "runtime intent verification is relaxed for this task")
		level = maxRiskLevel(level, "medium")
	case "off":
		factors = append(factors, "runtime intent verification is disabled for this task")
		level = maxRiskLevel(level, "high")
	}

	if len(env.ExpectedTools) == 0 && len(env.ExpectedEgress) == 0 {
		conflicts = append(conflicts, taskrisk.ConflictDetail{
			Field:       "action",
			Description: "task envelope does not declare any expected tools or egress targets",
			Severity:    "error",
		})
		level = "critical"
	}

	if len(env.ExpectedTools)+len(env.ExpectedEgress) >= 6 {
		factors = append(factors, "task envelope spans many different runtime operations")
		level = maxRiskLevel(level, "medium")
	}

	for _, item := range env.ExpectedTools {
		if item.InputRegex != "" {
			factors = append(factors, fmt.Sprintf("tool %q uses regex-based input matching", item.ToolName))
			level = maxRiskLevel(level, "medium")
		}
		for _, issue := range whyQualityIssues(item.Why) {
			conflicts = append(conflicts, taskrisk.ConflictDetail{
				Field:       "expected_use",
				Description: fmt.Sprintf("tool %q: %s", item.ToolName, issue),
				Severity:    "warning",
			})
			level = maxRiskLevel(level, "medium")
		}
	}

	for _, item := range env.ExpectedEgress {
		if item.PathRegex != "" {
			factors = append(factors, fmt.Sprintf("egress target %q uses regex-based path matching", item.Host))
			level = maxRiskLevel(level, "medium")
		}
		if item.Host == "*" || strings.HasPrefix(item.Host, "*.") {
			factors = append(factors, fmt.Sprintf("egress target %q is a wildcard host", item.Host))
			level = maxRiskLevel(level, "high")
		}
		switch strings.ToUpper(item.Method) {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			factors = append(factors, fmt.Sprintf("egress target %q allows mutating %s requests", item.Host, strings.ToUpper(item.Method)))
			level = maxRiskLevel(level, "high")
		}
		for _, issue := range whyQualityIssues(item.Why) {
			conflicts = append(conflicts, taskrisk.ConflictDetail{
				Field:       "expected_use",
				Description: fmt.Sprintf("egress target %q: %s", item.Host, issue),
				Severity:    "warning",
			})
			level = maxRiskLevel(level, "medium")
		}
	}

	explanation := "This task has a constrained runtime envelope."
	switch level {
	case "medium":
		explanation = "This task is moderately broad or relies on looser runtime matching, so it deserves review."
	case "high":
		explanation = "This task allows broad or mutating runtime activity, so misuse would have meaningful impact."
	case "critical":
		explanation = "This task envelope is incomplete or dangerously broad, so it should not be approved without revision."
	}

	return &taskrisk.RiskAssessment{
		RiskLevel:   level,
		Explanation: explanation,
		Factors:     uniqueStrings(factors),
		Conflicts:   conflicts,
	}
}

func maxRiskLevel(current, candidate string) string {
	order := map[string]int{
		"low":      0,
		"medium":   1,
		"high":     2,
		"critical": 3,
	}
	if order[candidate] > order[current] {
		return candidate
	}
	return current
}

func uniqueStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
