package policy

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
)

type ValidationIssue struct {
	Field   string
	Message string
}

func ValidateTaskEnvelope(env runtimetasks.Envelope) []ValidationIssue {
	var issues []ValidationIssue

	if len(env.ExpectedTools) == 0 && len(env.ExpectedEgress) == 0 {
		issues = append(issues, ValidationIssue{
			Field:   "expected_tools_json",
			Message: "at least one expected tool or expected egress item is required for a v2 task envelope",
		})
	}

	if env.IntentVerificationMode != "" &&
		env.IntentVerificationMode != "strict" &&
		env.IntentVerificationMode != "lenient" &&
		env.IntentVerificationMode != "off" {
		issues = append(issues, ValidationIssue{
			Field:   "intent_verification_mode",
			Message: "must be one of: strict, lenient, off",
		})
	}

	for i, item := range env.ExpectedTools {
		fieldPrefix := fmt.Sprintf("expected_tools_json[%d]", i)
		if strings.TrimSpace(item.ToolName) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".tool_name",
				Message: "tool_name is required",
			})
		}
		if strings.TrimSpace(item.Why) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".why",
				Message: "why is required",
			})
		}
		if item.InputRegex != "" {
			if _, err := regexp.Compile(item.InputRegex); err != nil {
				issues = append(issues, ValidationIssue{
					Field:   fieldPrefix + ".input_regex",
					Message: "must be a valid regular expression",
				})
			}
		}
	}

	for i, item := range env.ExpectedEgress {
		fieldPrefix := fmt.Sprintf("expected_egress_json[%d]", i)
		if strings.TrimSpace(item.Host) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".host",
				Message: "host is required",
			})
		}
		if strings.Contains(item.Host, "://") || strings.Contains(item.Host, "/") || strings.Contains(item.Host, " ") {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".host",
				Message: "host must be a bare hostname or wildcard host without scheme or path",
			})
		}
		if strings.TrimSpace(item.Why) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".why",
				Message: "why is required",
			})
		}
		if item.Method != "" {
			method := strings.ToUpper(item.Method)
			switch method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
			default:
				issues = append(issues, ValidationIssue{
					Field:   fieldPrefix + ".method",
					Message: "must be a valid HTTP method",
				})
			}
		}
		if item.Path != "" && item.PathRegex != "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".path_regex",
				Message: "path and path_regex are mutually exclusive",
			})
		}
		if item.PathRegex != "" {
			if _, err := regexp.Compile(item.PathRegex); err != nil {
				issues = append(issues, ValidationIssue{
					Field:   fieldPrefix + ".path_regex",
					Message: "must be a valid regular expression",
				})
			}
		}
	}

	return issues
}
