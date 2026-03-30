package yamlruntime

import (
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
)

// TransformFunc takes a raw value and returns a transformed value.
type TransformFunc func(v any) any

// builtinTransforms are the transforms available to all YAML adapters.
var builtinTransforms = map[string]TransformFunc{
	"money":    transformMoney,
	"upper":    transformUpper,
	"sanitize": transformSanitize,
}

// customTransforms are registered by Go code for adapter-specific logic
// (e.g. Notion's flattenProperties).
var customTransforms = map[string]TransformFunc{}

// RegisterTransform registers a named transform function available to YAML adapters.
func RegisterTransform(name string, fn TransformFunc) {
	customTransforms[name] = fn
}

// applyTransform looks up and applies a named transform.
func applyTransform(name string, v any) any {
	if fn, ok := builtinTransforms[name]; ok {
		return fn(v)
	}
	if fn, ok := customTransforms[name]; ok {
		return fn(v)
	}
	return v
}

// transformMoney converts a Stripe-style amount in smallest currency unit (cents) to a display string.
func transformMoney(v any) any {
	switch n := v.(type) {
	case float64:
		return fmt.Sprintf("%.2f", n/100.0)
	case int:
		return fmt.Sprintf("%.2f", float64(n)/100.0)
	case int64:
		return fmt.Sprintf("%.2f", float64(n)/100.0)
	case json_Number:
		f, err := n.Float64()
		if err != nil {
			return v
		}
		return fmt.Sprintf("%.2f", f/100.0)
	}
	return v
}

// json_Number is an alias to avoid importing encoding/json just for the type check.
// The JSON decoder with UseNumber produces json.Number values.
type json_Number = interface{ Float64() (float64, error) }

// transformUpper uppercases a string value.
func transformUpper(v any) any {
	if s, ok := v.(string); ok {
		return strings.ToUpper(s)
	}
	return v
}

// transformSanitize applies format.SanitizeText.
func transformSanitize(v any) any {
	if s, ok := v.(string); ok {
		return format.SanitizeText(s, format.MaxFieldLen)
	}
	return v
}
