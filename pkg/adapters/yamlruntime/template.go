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

	t, err := template.New("summary").Funcs(summaryFuncs).Parse(tmplStr)
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
