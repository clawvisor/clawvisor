package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// KnownConditionTypes lists condition types the evaluator understands.
var KnownConditionTypes = map[string]bool{
	"param_matches":        true,
	"param_not_contains":   true,
	"max_results_under":    true,
	"recipient_in_contacts": true,
}

// ValidatePolicy validates a parsed Policy and returns all errors found.
// The caller should also pass a set of known role IDs to validate the role field.
func ValidatePolicy(p *Policy, knownRoleIDs map[string]bool) []ValidationError {
	var errs []ValidationError

	if p.ID == "" {
		errs = append(errs, ValidationError{Field: "id", Message: "required"})
	} else if strings.ContainsAny(p.ID, " \t\n") {
		errs = append(errs, ValidationError{Field: "id", Message: "must not contain whitespace"})
	}

	if len(p.Rules) == 0 {
		errs = append(errs, ValidationError{Field: "rules", Message: "at least one rule is required"})
	}

	if p.Role != "" && knownRoleIDs != nil && !knownRoleIDs[p.Role] {
		errs = append(errs, ValidationError{
			Field:   "role",
			Message: fmt.Sprintf("unknown role %q — create it first via POST /api/roles", p.Role),
		})
	}

	for i, r := range p.Rules {
		prefix := fmt.Sprintf("rules[%d]", i)

		if r.Service == "" {
			errs = append(errs, ValidationError{Field: prefix + ".service", Message: "required"})
		}
		if len(r.Actions) == 0 {
			errs = append(errs, ValidationError{Field: prefix + ".actions", Message: "required; use [\"*\"] for all actions"})
		}
		if r.Allow == nil && !r.RequireApproval {
			errs = append(errs, ValidationError{
				Field:   prefix + ".allow",
				Message: "required unless require_approval is set",
			})
		}

		if r.Condition != nil {
			cerrs := validateCondition(r.Condition, prefix+".condition")
			errs = append(errs, cerrs...)
		}

		if r.TimeWindow != nil {
			terrs := validateTimeWindow(r.TimeWindow, prefix+".time_window")
			errs = append(errs, terrs...)
		}

		for j, f := range r.ResponseFilters {
			ferrs := validateFilter(f, fmt.Sprintf("%s.response_filters[%d]", prefix, j))
			errs = append(errs, ferrs...)
		}
	}

	return errs
}

func validateCondition(c *Condition, prefix string) []ValidationError {
	var errs []ValidationError

	if c.Type == "" {
		errs = append(errs, ValidationError{Field: prefix + ".type", Message: "required"})
		return errs
	}
	if !KnownConditionTypes[c.Type] {
		errs = append(errs, ValidationError{
			Field:   prefix + ".type",
			Message: fmt.Sprintf("unknown condition type %q", c.Type),
		})
		return errs
	}

	switch c.Type {
	case "param_matches":
		if c.Param == "" {
			errs = append(errs, ValidationError{Field: prefix + ".param", Message: "required for param_matches"})
		}
		if c.Pattern == "" {
			errs = append(errs, ValidationError{Field: prefix + ".pattern", Message: "required for param_matches"})
		} else if _, err := regexp.Compile(c.Pattern); err != nil {
			errs = append(errs, ValidationError{Field: prefix + ".pattern", Message: "invalid regex: " + err.Error()})
		}
	case "param_not_contains":
		if c.Param == "" {
			errs = append(errs, ValidationError{Field: prefix + ".param", Message: "required for param_not_contains"})
		}
		if c.Value == "" {
			errs = append(errs, ValidationError{Field: prefix + ".value", Message: "required for param_not_contains"})
		}
	case "max_results_under":
		if c.Param == "" {
			errs = append(errs, ValidationError{Field: prefix + ".param", Message: "required for max_results_under"})
		}
		if c.Max <= 0 {
			errs = append(errs, ValidationError{Field: prefix + ".max", Message: "must be a positive integer"})
		}
	// recipient_in_contacts has no required fields
	}

	return errs
}

func validateTimeWindow(tw *TimeWindow, prefix string) []ValidationError {
	var errs []ValidationError

	for _, d := range tw.Days {
		if _, ok := dayMap[strings.ToLower(d)]; !ok {
			errs = append(errs, ValidationError{
				Field:   prefix + ".days",
				Message: fmt.Sprintf("unknown day %q; use mon/tue/wed/thu/fri/sat/sun", d),
			})
		}
	}

	if tw.Hours != "" {
		if err := validateHoursRange(tw.Hours); err != nil {
			errs = append(errs, ValidationError{Field: prefix + ".hours", Message: err.Error()})
		}
	}

	return errs
}

func validateHoursRange(s string) error {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return fmt.Errorf("must be HH:MM-HH:MM format")
	}
	if err := validateHHMM(parts[0]); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := validateHHMM(parts[1]); err != nil {
		return fmt.Errorf("end: %w", err)
	}
	return nil
}

func validateHHMM(s string) error {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf("%q is not a valid HH:MM time", s)
	}
	return nil
}

func validateFilter(f ResponseFilter, prefix string) []ValidationError {
	var errs []ValidationError
	// Exactly one type must be set
	count := 0
	if f.Redact != "" {
		count++
	}
	if f.RedactRegex != "" {
		count++
		if _, err := regexp.Compile(f.RedactRegex); err != nil {
			errs = append(errs, ValidationError{Field: prefix + ".redact_regex", Message: "invalid regex: " + err.Error()})
		}
	}
	if f.RemoveField != "" {
		count++
	}
	if f.TruncateField != "" {
		count++
		if f.MaxChars <= 0 {
			errs = append(errs, ValidationError{Field: prefix + ".max_chars", Message: "must be positive when truncate_field is set"})
		}
	}
	if f.Semantic != "" {
		count++
	}
	if count == 0 {
		errs = append(errs, ValidationError{Field: prefix, Message: "filter must specify one of: redact, redact_regex, remove_field, truncate_field, semantic"})
	} else if count > 1 {
		errs = append(errs, ValidationError{Field: prefix, Message: "filter must specify exactly one type, got multiple"})
	}
	return errs
}
