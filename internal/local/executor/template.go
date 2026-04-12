package executor

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// InterpolateTemplate replaces {{param_name}} placeholders in a template string
// with corresponding parameter values. Only declared params are replaced.
func InterpolateTemplate(template string, params map[string]string, declaredParams []services.Param, format string) string {
	if template == "" {
		return ""
	}

	// Build set of declared param names.
	declared := make(map[string]bool, len(declaredParams))
	for _, p := range declaredParams {
		declared[p.Name] = true
	}

	result := template
	for name, value := range params {
		if !declared[name] {
			continue
		}
		placeholder := "{{" + name + "}}"
		var replacement string
		if format == "json" {
			replacement = jsonEscape(value)
		} else {
			replacement = value
		}
		result = strings.ReplaceAll(result, placeholder, replacement)
	}

	return result
}

// jsonEscape escapes a string for safe inclusion in a JSON string value.
func jsonEscape(s string) string {
	// Marshal produces a quoted JSON string; strip the surrounding quotes.
	b, _ := json.Marshal(s)
	// Remove leading and trailing "
	return string(b[1 : len(b)-1])
}
