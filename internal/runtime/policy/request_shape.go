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

var alphanumericWhitelist = regexp.MustCompile(`^[a-zA-Z0-9 ._:\-]+$`)

func ValidateTaskEnvelope(env runtimetasks.Envelope) []ValidationIssue {
	var issues []ValidationIssue

	if len(env.ExpectedTools) == 0 && len(env.ExpectedEgress) == 0 {
		issues = append(issues, ValidationIssue{
			Field:   "expected_tools",
			Message: "at least one expected tool or expected egress item is required for a v2 task envelope",
		})
	}

	issues = append(issues, validateTaskEnvelopeBody(env)...)
	return issues
}

// ValidateTaskEnvelopeAdditions validates the per-item structure of an
// expansion envelope without requiring expected_tools OR expected_egress
// at the top level. A credentials-only expansion ("I now need this vault
// item for the already-approved tool/egress path") is a legitimate ask
// that ValidateTaskEnvelope would reject. Callers (the Expand handler)
// already gate emptiness via envelopeHasAdditions, so the top-level
// non-empty check is redundant on the expansion path.
func ValidateTaskEnvelopeAdditions(env runtimetasks.Envelope) []ValidationIssue {
	return validateTaskEnvelopeBody(env)
}

// validateTaskEnvelopeBody runs the per-item structural checks shared
// by ValidateTaskEnvelope (create) and ValidateTaskEnvelopeAdditions
// (expand). It does NOT enforce the "at least one tool or egress"
// constraint — that's the create-path's call.
func validateTaskEnvelopeBody(env runtimetasks.Envelope) []ValidationIssue {
	var issues []ValidationIssue

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
		fieldPrefix := fmt.Sprintf("expected_tools[%d]", i)
		if strings.TrimSpace(item.ToolName) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".tool_name",
				Message: "tool_name is required",
			})
		} else if !alphanumericWhitelist.MatchString(item.ToolName) {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".tool_name",
				Message: "must contain only alphanumeric characters, spaces, dots, colons, dashes, and underscores",
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
		fieldPrefix := fmt.Sprintf("expected_egress[%d]", i)
		if strings.TrimSpace(item.Host) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".host",
				Message: "host is required",
			})
		}
		if strings.Contains(item.Host, "://") || strings.Contains(item.Host, "/") || strings.Contains(item.Host, " ") || strings.Contains(item.Host, ":") {
			// The `:` check forces "api.github.com" and "api.github.com:443"
			// to land at the same canonical form. Without it, an expansion
			// adding "api.github.com:443" while the parent has
			// "api.github.com" splits the `why` across two entries the
			// agent thinks are the same — the dashboard would render two
			// distinct rows and the dedup map would never collapse them.
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".host",
				Message: "host must be a bare hostname or wildcard host without scheme, port, or path",
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

	issues = append(issues, ValidateRequiredCredentials(env.RequiredCredentials)...)

	return issues
}

func ValidateRequiredCredentials(required []runtimetasks.RequiredCredential) []ValidationIssue {
	var issues []ValidationIssue
	for i, item := range required {
		fieldPrefix := fmt.Sprintf("required_credentials[%d]", i)
		idSet := strings.TrimSpace(item.VaultItemID) != ""
		handleSet := strings.TrimSpace(item.VaultItemHandle) != ""
		if !idSet && !handleSet {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".vault_item_id",
				Message: "vault_item_id or vault_item_handle is required",
			})
		}
		if idSet && !alphanumericWhitelist.MatchString(item.VaultItemID) {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".vault_item_id",
				Message: "must contain only alphanumeric characters, spaces, dots, colons, dashes, and underscores",
			})
		}
		if handleSet && !alphanumericWhitelist.MatchString(item.VaultItemHandle) {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".vault_item_handle",
				Message: "must contain only alphanumeric characters, spaces, dots, colons, dashes, and underscores",
			})
		}
		if idSet && handleSet {
			// Mutual exclusion: the merge dedup keys credentials by
			// identifier kind (id vs. handle), so an entry that sets
			// BOTH would be ambiguous about which kind it belongs to —
			// and the runtime resolver would only honor vault_item_id
			// (id-first fallback), silently dropping the handle the
			// agent supplied.
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".vault_item_handle",
				Message: "vault_item_id and vault_item_handle are mutually exclusive; set exactly one",
			})
		}
		if strings.TrimSpace(item.Why) == "" {
			issues = append(issues, ValidationIssue{
				Field:   fieldPrefix + ".why",
				Message: "why is required",
			})
		}
	}
	return issues
}
