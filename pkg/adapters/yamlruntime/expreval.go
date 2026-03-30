package yamlruntime

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// compiledAction holds pre-compiled expr programs for a single action.
type compiledAction struct {
	fieldExprs      map[int]*vm.Program    // response field index → compiled expr
	paramTransforms map[string]*vm.Program // param name → compiled transform expr
	paramDefaults   map[string]*vm.Program // param name → compiled default_expr
}

// compileOptions are shared expr compilation options.
var compileOptions = []expr.Option{
	expr.AllowUndefinedVariables(),
	expr.Function("sanitize", sanitizeFunc, new(func(string, ...int) string)),
	expr.Function("rfc3339", rfc3339Func, new(func(string) string)),
	expr.Function("endOfDay", endOfDayFunc, new(func(string) string)),
	expr.Function("isAllDay", isAllDayFunc, new(func(string) bool)),
	expr.Function("now", nowFunc, new(func() string)),
	expr.Function("replace", replaceFunc, new(func(string, string, string) string)),
	expr.Function("emailList", emailListFunc),
	expr.Function("findHeader", findHeaderFunc),
	expr.Function("extractMimeBody", extractMimeBodyFunc),
	expr.Function("base64Decode", base64DecodeFunc),
	expr.Function("stripHTML", stripHTMLFunc),
}

// compileAction compiles all expr expressions in an action definition.
// Returns nil (not an error) if the action has no expressions.
func compileAction(action yamldef.Action) (*compiledAction, error) {
	ca := &compiledAction{
		fieldExprs:      map[int]*vm.Program{},
		paramTransforms: map[string]*vm.Program{},
		paramDefaults:   map[string]*vm.Program{},
	}

	// Compile response field expressions.
	for i, f := range action.Response.Fields {
		if f.Expr == "" {
			continue
		}
		prog, err := expr.Compile(f.Expr, compileOptions...)
		if err != nil {
			return nil, fmt.Errorf("field %q expr: %w", f.Name, err)
		}
		ca.fieldExprs[i] = prog
	}

	// Compile param transforms and dynamic defaults.
	for name, p := range action.Params {
		if p.Transform != "" {
			prog, err := expr.Compile(p.Transform, compileOptions...)
			if err != nil {
				return nil, fmt.Errorf("param %q transform: %w", name, err)
			}
			ca.paramTransforms[name] = prog
		}
		if p.DefaultExpr != "" {
			prog, err := expr.Compile(p.DefaultExpr, compileOptions...)
			if err != nil {
				return nil, fmt.Errorf("param %q default_expr: %w", name, err)
			}
			ca.paramDefaults[name] = prog
		}
	}

	if len(ca.fieldExprs) == 0 && len(ca.paramTransforms) == 0 && len(ca.paramDefaults) == 0 {
		return nil, nil // no expressions to compile
	}

	return ca, nil
}

// evalExpr runs a compiled program against a data map.
func evalExpr(prog *vm.Program, data map[string]any) (any, error) {
	return expr.Run(prog, data)
}

// ── Custom functions ────────────────────────────────────────────────────────

func sanitizeFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return "", nil
	}
	s, ok := params[0].(string)
	if !ok {
		return fmt.Sprintf("%v", params[0]), nil
	}
	maxLen := format.MaxFieldLen
	if len(params) > 1 {
		if n, ok := toAnyInt(params[1]); ok {
			maxLen = n
		}
	}
	return format.SanitizeText(s, maxLen), nil
}

func rfc3339Func(params ...any) (any, error) {
	if len(params) == 0 {
		return "", nil
	}
	s, ok := params[0].(string)
	if !ok {
		return "", fmt.Errorf("rfc3339: expected string, got %T", params[0])
	}
	if s == "" {
		return "", nil
	}
	// Already RFC3339-ish (has time component).
	if len(s) > 10 {
		return s, nil
	}
	// Plain date "YYYY-MM-DD" → start of day UTC.
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s, nil // pass through; API will reject with a clear error
	}
	return t.UTC().Format(time.RFC3339), nil
}

func endOfDayFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return "", nil
	}
	s, ok := params[0].(string)
	if !ok {
		return "", fmt.Errorf("endOfDay: expected string, got %T", params[0])
	}
	if s == "" {
		return "", nil
	}
	if len(s) > 10 {
		return s, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s, nil
	}
	return t.Add(24*time.Hour - time.Second).UTC().Format(time.RFC3339), nil
}

func isAllDayFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return false, nil
	}
	s, ok := params[0].(string)
	if !ok {
		return false, nil
	}
	return len(s) == 10, nil
}

func nowFunc(params ...any) (any, error) {
	return time.Now().UTC().Format(time.RFC3339), nil
}

// emailList transforms a list of strings into [{email: s}, ...].
// Used for Google Calendar attendees.
func emailListFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return nil, nil
	}
	list, ok := params[0].([]any)
	if !ok {
		return nil, nil
	}
	result := make([]any, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			result = append(result, map[string]any{"email": s})
		}
	}
	return result, nil
}

func replaceFunc(params ...any) (any, error) {
	if len(params) < 3 {
		return "", fmt.Errorf("replace: requires 3 arguments (s, old, new)")
	}
	s, _ := params[0].(string)
	old, _ := params[1].(string)
	new_, _ := params[2].(string)
	return strings.ReplaceAll(s, old, new_), nil
}

// findHeader searches a list of {name, value} header maps for the first
// matching header (case-insensitive) and returns its value.
// Usage: findHeader(payload.headers, "From")
func findHeaderFunc(params ...any) (any, error) {
	if len(params) < 2 {
		return "", fmt.Errorf("findHeader: requires 2 arguments (headers, name)")
	}
	headers, ok := params[0].([]any)
	if !ok {
		return "", nil
	}
	target, _ := params[1].(string)
	target = strings.ToLower(target)
	for _, h := range headers {
		m, ok := h.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if strings.ToLower(name) == target {
			val, _ := m["value"].(string)
			return val, nil
		}
	}
	return "", nil
}

// extractMimeBody walks a Gmail message payload (as map[string]any) to extract
// the text body. Prefers text/plain, falls back to text/html with HTML stripping.
// Usage: extractMimeBody(payload)
func extractMimeBodyFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return "", nil
	}
	payload, ok := params[0].(map[string]any)
	if !ok {
		return "", nil
	}
	return mimeWalk(payload), nil
}

// mimeWalk recursively walks a Gmail payload map to find a text body.
func mimeWalk(payload map[string]any) string {
	mimeType, _ := payload["mimeType"].(string)
	bodyData := getBodyData(payload)

	// Direct text/plain body.
	if mimeType == "text/plain" && bodyData != "" {
		return decodeBase64URL(bodyData)
	}

	// Search parts for text/plain.
	parts := getPartsSlice(payload)
	for _, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		partMime, _ := pm["mimeType"].(string)
		partBody := getBodyData(pm)
		if partMime == "text/plain" && partBody != "" {
			return decodeBase64URL(partBody)
		}
		// Nested multipart (one level deeper).
		subParts := getPartsSlice(pm)
		for _, sub := range subParts {
			sm, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			subMime, _ := sm["mimeType"].(string)
			subBody := getBodyData(sm)
			if subMime == "text/plain" && subBody != "" {
				return decodeBase64URL(subBody)
			}
		}
	}

	// Fall back to text/html with HTML stripping.
	for _, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		partMime, _ := pm["mimeType"].(string)
		partBody := getBodyData(pm)
		if partMime == "text/html" && partBody != "" {
			return doStripHTML(decodeBase64URL(partBody))
		}
	}
	if mimeType == "text/html" && bodyData != "" {
		return doStripHTML(decodeBase64URL(bodyData))
	}

	return ""
}

func getBodyData(m map[string]any) string {
	body, ok := m["body"].(map[string]any)
	if !ok {
		return ""
	}
	data, _ := body["data"].(string)
	return data
}

func getPartsSlice(m map[string]any) []any {
	parts, _ := m["parts"].([]any)
	return parts
}

// base64Decode decodes a URL-safe base64 string (as used by Gmail API).
// Usage: base64Decode(body.data)
func base64DecodeFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return "", nil
	}
	s, ok := params[0].(string)
	if !ok {
		return "", nil
	}
	return decodeBase64URL(s), nil
}

// decodeBase64URL decodes a URL-safe base64 string, falling back to standard encoding.
func decodeBase64URL(s string) string {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

// stripHTML removes HTML tags, style/script blocks, and decodes common entities.
// Usage: stripHTML(htmlString)
func stripHTMLFunc(params ...any) (any, error) {
	if len(params) == 0 {
		return "", nil
	}
	s, ok := params[0].(string)
	if !ok {
		return "", nil
	}
	return doStripHTML(s), nil
}

// doStripHTML is the implementation shared by stripHTMLFunc and extractMimeBody fallback.
func doStripHTML(s string) string {
	// Remove <style>...</style> and <script>...</script> blocks.
	for _, tag := range []string{"style", "script"} {
		for {
			open := strings.Index(strings.ToLower(s), "<"+tag)
			if open < 0 {
				break
			}
			close := strings.Index(strings.ToLower(s[open:]), "</"+tag+">")
			if close < 0 {
				s = s[:open]
				break
			}
			s = s[:open] + s[open+close+len("</"+tag+">"):]
		}
	}
	// Strip remaining HTML tags.
	var out strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			out.WriteRune(' ')
		case !inTag:
			out.WriteRune(r)
		}
	}
	// Decode common HTML entities.
	result := out.String()
	result = strings.ReplaceAll(result, "&amp;", "&")
	result = strings.ReplaceAll(result, "&lt;", "<")
	result = strings.ReplaceAll(result, "&gt;", ">")
	result = strings.ReplaceAll(result, "&quot;", `"`)
	result = strings.ReplaceAll(result, "&#39;", "'")
	result = strings.ReplaceAll(result, "&nbsp;", " ")
	// Collapse whitespace/empty lines.
	lines := strings.Split(result, "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func toAnyInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
