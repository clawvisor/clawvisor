package handlers

import (
	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpadapter"
)

// applyMCPResponseMiddleware is the post-Execute middleware demonstrating the
// architectural inversion: response sanitization, HTML stripping, and field
// truncation that would otherwise live as per-adapter YAML transforms now run
// in the gateway, generically, against any MCP-backed result.
//
// Widening this to every adapter is a separate change — existing YAML
// adapters already do their own per-action transforms, so gate on the
// concrete type for now to keep the blast radius contained.
func applyMCPResponseMiddleware(adapter adapters.Adapter, result *adapters.Result) {
	if _, ok := adapter.(*mcpadapter.MCPAdapter); !ok {
		return
	}
	if result == nil {
		return
	}
	result.Data = sanitizeAny(result.Data, 0)
}

const maxRecursionDepth = 8

// sanitizeAny walks an arbitrarily-shaped JSON value, applying:
//   - HTML strip + dangerous-Unicode strip + per-field truncation on strings
//   - array length cap
//   - secret-key removal on map keys
//   - recursion limit (defensive against pathological nesting)
func sanitizeAny(v any, depth int) any {
	if depth >= maxRecursionDepth {
		return "[depth-limited]"
	}
	switch x := v.(type) {
	case string:
		return format.SanitizeText(x, format.MaxBodyLen)
	case []any:
		x = format.TruncateSlice(x, format.MaxArrayItems)
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = sanitizeAny(item, depth+1)
		}
		return out
	case map[string]any:
		x = format.StripSecrets(x)
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = sanitizeAny(val, depth+1)
		}
		return out
	default:
		return v
	}
}
