// Package filters applies policy ResponseFilters to adapter results.
// Structural filters are applied deterministically; semantic filters are executed
// via an optional LLM function (nil = log as deferred, no-op).
package filters

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
)

// FilterRecord is appended to the audit log for each filter applied.
type FilterRecord struct {
	Type      string `json:"type"`             // "structural" | "semantic"
	Filter    string `json:"filter"`           // e.g. "redact:email_address"
	Matches   int    `json:"matches,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error,omitempty"`
}

// SemanticApplyFn is called for each semantic filter when an LLM is configured.
// It returns the modified result and whether the filter was applied.
// A nil fn means LLM is not configured (semantic filters are skipped).
type SemanticApplyFn func(ctx context.Context, instruction string, result *adapters.Result) (*adapters.Result, bool)

// Apply applies all filters to result without LLM support (semantic filters are no-ops).
func Apply(result *adapters.Result, responseFilters []ResponseFilter) (*adapters.Result, []FilterRecord) {
	return ApplyContext(context.Background(), result, responseFilters, nil)
}

// ApplyContext applies all filters with optional LLM support for semantic filters.
func ApplyContext(ctx context.Context, result *adapters.Result, responseFilters []ResponseFilter, semanticFn SemanticApplyFn) (*adapters.Result, []FilterRecord) {
	if len(responseFilters) == 0 {
		return result, nil
	}

	// Work on a JSON round-trip copy so we can manipulate the Data field generically.
	data := toMap(result.Data)
	var log []FilterRecord

	for _, f := range responseFilters {
		if f.IsSemantic() {
			if semanticFn == nil {
				log = append(log, FilterRecord{
					Type:    "semantic",
					Filter:  "semantic:" + truncate(f.Semantic, 50),
					Applied: false,
					Error:   "semantic filters deferred (no LLM configured)",
				})
				continue
			}
			// Call LLM semantic filter.
			cur := &adapters.Result{Summary: result.Summary, Data: data}
			filtered, applied := semanticFn(ctx, f.Semantic, cur)
			rec := FilterRecord{
				Type:    "semantic",
				Filter:  "semantic:" + truncate(f.Semantic, 50),
				Applied: applied,
			}
			if applied && filtered != nil {
				data = toMap(filtered.Data)
				if filtered.Summary != "" {
					result = &adapters.Result{Summary: filtered.Summary, Data: data}
				}
			}
			log = append(log, rec)
			continue
		}

		rec, modified := applyStructural(data, f)
		data = modified
		log = append(log, rec)
	}

	out := &adapters.Result{
		Summary: result.Summary,
		Data:    data,
	}
	return out, log
}

func applyStructural(data map[string]any, f ResponseFilter) (FilterRecord, map[string]any) {
	switch {
	case f.Redact != "":
		pattern := builtinPattern(f.Redact)
		if pattern == "" {
			return FilterRecord{Type: "structural", Filter: "redact:" + f.Redact, Applied: false, Error: "unknown built-in pattern"}, data
		}
		matches := redactInMap(data, regexp.MustCompile(pattern))
		return FilterRecord{Type: "structural", Filter: "redact:" + f.Redact, Matches: matches, Applied: true}, data

	case f.RedactRegex != "":
		re, err := regexp.Compile(f.RedactRegex)
		if err != nil {
			return FilterRecord{Type: "structural", Filter: "redact_regex", Applied: false, Error: err.Error()}, data
		}
		matches := redactInMap(data, re)
		return FilterRecord{Type: "structural", Filter: "redact_regex:" + truncate(f.RedactRegex, 30), Matches: matches, Applied: true}, data

	case f.RemoveField != "":
		removed := removeField(data, f.RemoveField)
		return FilterRecord{Type: "structural", Filter: "remove_field:" + f.RemoveField, Applied: removed}, data

	case f.TruncateField != "":
		truncated := truncateField(data, f.TruncateField, f.MaxChars)
		return FilterRecord{Type: "structural", Filter: fmt.Sprintf("truncate_field:%s@%d", f.TruncateField, f.MaxChars), Truncated: truncated, Applied: true}, data
	}

	return FilterRecord{Type: "structural", Filter: "unknown", Applied: false, Error: "unrecognized filter"}, data
}

// ── Built-in redaction patterns ───────────────────────────────────────────────

func builtinPattern(name string) string {
	switch name {
	case "email_address":
		return `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`
	case "phone_number":
		return `(\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}`
	case "credit_card":
		return `\b(?:\d[ -]?){13,16}\b`
	case "ssn":
		return `\b\d{3}[-]?\d{2}[-]?\d{4}\b`
	case "api_key":
		return `(?i)(api[_-]?key|token)[=:\s]["']?[a-zA-Z0-9_\-]{16,}`
	case "bearer_token":
		return `(?i)bearer\s+[a-zA-Z0-9_\-\.]+`
	}
	return ""
}

// ── Field operations ──────────────────────────────────────────────────────────

// removeField removes a dot-notation field from the map. Returns true if the field was found.
func removeField(data map[string]any, path string) bool {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		_, found := data[path]
		delete(data, path)
		return found
	}
	if nested, ok := data[parts[0]].(map[string]any); ok {
		return removeField(nested, parts[1])
	}
	return false
}

// truncateField truncates a string field in data at maxChars. Returns true if truncated.
func truncateField(data map[string]any, path string, maxChars int) bool {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		if v, ok := data[path].(string); ok && len(v) > maxChars {
			data[path] = v[:maxChars] + " [truncated]"
			return true
		}
		return false
	}
	if nested, ok := data[parts[0]].(map[string]any); ok {
		return truncateField(nested, parts[1], maxChars)
	}
	return false
}

// redactInMap replaces all regex matches in all string values with [redacted].
func redactInMap(data map[string]any, re *regexp.Regexp) int {
	total := 0
	for k, v := range data {
		switch val := v.(type) {
		case string:
			replaced := re.ReplaceAllStringFunc(val, func(s string) string {
				total++
				return "[redacted]"
			})
			data[k] = replaced
		case map[string]any:
			total += redactInMap(val, re)
		case []any:
			for i, item := range val {
				if s, ok := item.(string); ok {
					replaced := re.ReplaceAllStringFunc(s, func(m string) string {
						total++
						return "[redacted]"
					})
					val[i] = replaced
				} else if m, ok := item.(map[string]any); ok {
					total += redactInMap(m, re)
				}
			}
		}
	}
	return total
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// toMap converts any value to map[string]any via JSON round-trip.
func toMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		// If data is an array or scalar, wrap it
		return map[string]any{"data": v}
	}
	return m
}

func truncate(s string, max int) string {
	return format.SanitizeText(s, max)
}
