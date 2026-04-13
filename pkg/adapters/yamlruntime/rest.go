package yamlruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// executeREST executes a REST action as defined in the YAML spec.
func executeREST(ctx context.Context, client *http.Client, baseURL string, action yamldef.Action, params map[string]any, credFields map[string]string, ca *compiledAction) (*adapters.Result, error) {
	// Resolve path-parameter defaults before interpolation so that callers
	// who omit a path param with a default (e.g. calendar_id="primary") still
	// get the placeholder replaced.
	pathParams := make(map[string]any, len(params))
	for k, v := range params {
		pathParams[k] = v
	}
	for name, paramDef := range action.Params {
		if paramDef.Location != "path" {
			continue
		}
		if _, ok := pathParams[name]; ok {
			continue // caller provided it
		}
		val, _ := resolveParamWithExpr(params, name, paramDef, ca)
		if val != nil {
			pathParams[name] = val
		}
	}

	// Build the URL path with parameter interpolation.
	path := interpolatePath(action.Path, pathParams, credFields)
	if unresolved := findUnresolvedPlaceholders(path); len(unresolved) > 0 {
		return nil, fmt.Errorf("missing required path parameter(s): %s", strings.Join(unresolved, ", "))
	}
	fullURL := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")

	// Separate params by location.
	queryParams := url.Values{}
	bodyParams := map[string]any{}

	for name, paramDef := range action.Params {
		val, provided := resolveParamWithExpr(params, name, paramDef, ca)
		if val == nil {
			continue
		}
		// In sparse body mode, skip unprovided body params.
		if !provided && action.BodyMode == "sparse" && paramDef.Location == "body" {
			continue
		}

		apiKey := name
		if paramDef.MapTo != "" {
			apiKey = paramDef.MapTo
		}
		switch paramDef.Location {
		case "query":
			queryParams.Set(apiKey, fmt.Sprintf("%v", val))
		case "body":
			bodyParams[apiKey] = val
		case "path":
			// Already handled above.
		}
	}

	if len(queryParams) > 0 {
		fullURL += "?" + queryParams.Encode()
	}

	// Build request body.
	var bodyReader io.Reader
	var contentType string
	if action.Method != "GET" && action.Method != "DELETE" && len(bodyParams) > 0 {
		encoding := action.Encoding
		if encoding == "" {
			encoding = "json"
		}
		switch encoding {
		case "json":
			b, err := json.Marshal(bodyParams)
			if err != nil {
				return nil, fmt.Errorf("marshaling request body: %w", err)
			}
			bodyReader = bytes.NewReader(b)
			contentType = "application/json"
		case "form":
			form := url.Values{}
			for k, v := range bodyParams {
				form.Set(k, fmt.Sprintf("%v", v))
			}
			bodyReader = strings.NewReader(form.Encode())
			contentType = "application/x-www-form-urlencoded"
		}
	}

	req, err := http.NewRequestWithContext(ctx, action.Method, fullURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	// Parse the JSON response.
	var raw any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}
	}

	// Check for API-level errors (e.g. Slack's {ok: false, error: "..."}).
	if action.ErrorCheck != nil {
		if err := checkResponseError(raw, action.ErrorCheck); err != nil {
			return nil, err
		}
	}

	// Extract data from the response.
	data := extractData(raw, action.Response, ca)
	summary := renderSummary(action.Response.Summary, data)

	return &adapters.Result{
		Summary: summary,
		Data:    data,
	}, nil
}

// interpolatePath replaces {{.param}} and {{.credential.field}} placeholders in the URL path.
func interpolatePath(path string, params map[string]any, credFields map[string]string) string {
	result := path
	for k, v := range params {
		result = strings.ReplaceAll(result, "{{."+k+"}}", escapePathValue(fmt.Sprintf("%v", v)))
	}
	for k, v := range credFields {
		result = strings.ReplaceAll(result, "{{.credential."+k+"}}", escapePathValue(v))
	}
	return result
}

// escapePathValue encodes reserved characters inside a path placeholder while
// preserving literal "/" so adapters can keep using multi-segment resource
// names like "people/123".
func escapePathValue(v string) string {
	return strings.ReplaceAll(url.PathEscape(v), "%2F", "/")
}

// resolveParamWithExpr extracts a parameter value from the input, applying
// defaults (static or expr), int constraints, and transform expressions.
// Returns (value, provided) where provided indicates the caller explicitly
// supplied this param (used for body_mode: sparse).
func resolveParamWithExpr(params map[string]any, name string, def yamldef.Param, ca *compiledAction) (any, bool) {
	val, ok := params[name]
	provided := ok && val != nil

	if !ok || val == nil {
		// Try dynamic default first, then static.
		if ca != nil {
			if prog, has := ca.paramDefaults[name]; has {
				result, err := evalExpr(prog, params)
				if err == nil && result != nil {
					val = result
					ok = true
				}
			}
		}
		if !ok && def.Default != nil {
			val = def.Default
			ok = true
		}
		if !ok {
			return nil, false
		}
	}

	// Apply int constraints.
	if def.Type == "int" {
		intVal := toInt(val)
		if def.Min != nil && intVal < *def.Min {
			intVal = *def.Min
		}
		if def.Max != nil && intVal > *def.Max {
			intVal = *def.Max
		}
		val = intVal
	}

	// Apply transform expression.
	if ca != nil {
		if prog, has := ca.paramTransforms[name]; has {
			env := map[string]any{}
			for k, v := range params {
				env[k] = v
			}
			env[name] = val // ensure the (possibly defaulted) value is available
			result, err := evalExpr(prog, env)
			if err == nil {
				val = result
			}
		}
	}

	return val, provided
}

// checkResponseError checks for API-level errors in the response body.
func checkResponseError(raw any, check *yamldef.ErrorCheckDef) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if check.SuccessPath != "" {
		if success, ok := navigatePath(m, check.SuccessPath); ok {
			if b, ok := success.(bool); ok && !b {
				errMsg := "unknown error"
				if check.ErrorPath != "" {
					if e, ok := navigatePath(m, check.ErrorPath); ok {
						errMsg = fmt.Sprintf("%v", e)
					}
				}
				return fmt.Errorf("API error: %s", errMsg)
			}
		}
	}
	return nil
}

// extractData navigates to the data_path in the response and extracts fields.
func extractData(raw any, respDef yamldef.ResponseDef, ca *compiledAction) any {
	if raw == nil {
		return nil
	}

	// Navigate to data_path.
	data := raw
	if respDef.DataPath != "" {
		if m, ok := raw.(map[string]any); ok {
			if found, ok := navigatePath(m, respDef.DataPath); ok {
				data = found
			}
		}
	}

	if len(respDef.Fields) == 0 {
		return data
	}

	// Handle array of objects.
	if arr, ok := data.([]any); ok {
		items := make([]map[string]any, 0, len(arr))
		for _, item := range arr {
			if len(items) >= format.MaxArrayItems {
				break
			}
			if obj, ok := item.(map[string]any); ok {
				items = append(items, extractFields(obj, respDef.Fields, ca))
			}
		}
		return items
	}

	// Handle single object.
	if obj, ok := data.(map[string]any); ok {
		return extractFields(obj, respDef.Fields, ca)
	}

	return data
}

// extractFields extracts and transforms the specified fields from a JSON object.
func extractFields(obj map[string]any, fields []yamldef.FieldDef, ca *compiledAction) map[string]any {
	result := make(map[string]any, len(fields))
	for i, f := range fields {
		outputKey := f.Name
		if f.Rename != "" {
			outputKey = f.Rename
		}

		var val any

		// Expr takes precedence over path/name lookup.
		if ca != nil {
			if prog, ok := ca.fieldExprs[i]; ok {
				v, err := evalExpr(prog, obj)
				if err == nil {
					val = v
				}
				if val == nil && f.Optional {
					continue // omit optional nil fields
				}
				if val == nil && f.Nullable {
					result[outputKey] = ""
					continue
				}
				if f.Sanitize {
					if s, ok := val.(string); ok {
						val = format.SanitizeText(s, format.MaxFieldLen)
					}
				}
				if f.Transform != "" {
					val = applyTransform(f.Transform, val)
				}
				result[outputKey] = val
				continue
			}
		}

		if f.Path != "" {
			val, _ = navigatePath(obj, f.Path)
		} else {
			val = obj[f.Name]
		}

		if val == nil && f.Optional {
			continue
		}

		if val == nil && f.Nullable {
			result[outputKey] = ""
			continue
		}

		if f.Sanitize {
			if s, ok := val.(string); ok {
				val = format.SanitizeText(s, format.MaxFieldLen)
			}
		}

		if f.Transform != "" {
			val = applyTransform(f.Transform, val)
		}

		result[outputKey] = val
	}
	return result
}

// navigatePath follows a dot-delimited path through nested maps.
// e.g. "state.name" on {"state": {"name": "Done"}} returns "Done".
func navigatePath(obj map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var current any = obj
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// validateRequiredParams checks that all required params are present.
func validateRequiredParams(params map[string]any, paramDefs map[string]yamldef.Param, serviceID, actionName string) error {
	for name, def := range paramDefs {
		if def.Required {
			val, ok := params[name]
			if !ok || val == nil || val == "" {
				return fmt.Errorf("%s %s: %s is required", serviceID, actionName, name)
			}
		}
	}
	return nil
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// findUnresolvedPlaceholders returns the names of any {{.xxx}} placeholders
// still present in path after interpolation. An empty slice means all
// placeholders were resolved.
func findUnresolvedPlaceholders(path string) []string {
	var names []string
	for rest := path; ; {
		start := strings.Index(rest, "{{.")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			break
		}
		name := rest[start+3 : start+end]
		names = append(names, name)
		rest = rest[start+end+2:]
	}
	return names
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
