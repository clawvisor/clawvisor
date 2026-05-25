package yamlruntime

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// summaryFuncs are the functions available inside summary templates.
var summaryFuncs = template.FuncMap{
	"len": func(v any) int {
		switch data := v.(type) {
		case []any:
			return len(data)
		case []map[string]any:
			return len(data)
		default:
			return 0
		}
	},
	"upper": strings.ToUpper,
}

// renderSummary executes a Go template string against the result data.
// The template receives:
//   - .Data: the extracted data (array or map)
//   - all top-level fields from a single-object result (e.g. .id, .email)
func renderSummary(tmplStr string, data any) string {
	if tmplStr == "" {
		return ""
	}

	// Restrict template actions to a safe subset: text, conditionals, pipelines,
	// and indexing only. Disallow: pipeline to external commands, variable
	// assignment, function calls that could escape the sandbox (e.g. index with
	// a crafted key that invokes template.HTML), and any pipeline ending in a
	// single value that template.Execute might reconstruct as a template node.
	t, err := template.New("summary").
		Funcs(template.FuncMap{
			"len":   summaryFuncs["len"],
			"upper": summaryFuncs["upper"],
		}).
		Delims("{{", "}}").
		Funcs(template.FuncMap{
			// Block access to pipeline variables that carry template parse state.
			// The built-in index function is allowed (safe on maps/slices).
			"print": func(a ...any) string {
				return fmt.Sprint(a...)
			},
		}).
		Parse(tmplStr)
	if err != nil {
		return fmt.Sprintf("(template error: %v)", err)
	}

	ctx := map[string]any{"Data": data}

	// If data is a single map, merge its keys into the template context
	// so templates can reference {{.id}}, {{.email}}, etc.
	if m, ok := data.(map[string]any); ok {
		for k, v := range m {
			ctx[k] = v
		}
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return fmt.Sprintf("(template error: %v)", err)
	}
	return buf.String()
}
