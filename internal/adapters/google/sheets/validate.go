package sheets

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	spreadsheetIDRe = regexp.MustCompile(`^[a-zA-Z0-9-_]{10,200}$`)
	// Permissive A1 notation check. We don't try to fully parse it, but we do
	// prevent obvious garbage and path traversal-like strings.
	a1RangeRe = regexp.MustCompile(`^[^\s]{1,200}$`)
)

func paramInt(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func requireSpreadsheetID(params map[string]any) (string, error) {
	ssid, _ := params["spreadsheet_id"].(string)
	ssid = strings.TrimSpace(ssid)
	if ssid == "" {
		return "", fmt.Errorf("spreadsheet_id is required")
	}
	if !spreadsheetIDRe.MatchString(ssid) {
		return "", fmt.Errorf("spreadsheet_id looks invalid")
	}
	return ssid, nil
}

func requireA1Range(params map[string]any) (string, error) {
	rng, _ := params["range"].(string)
	rng = strings.TrimSpace(rng)
	if rng == "" {
		return "", fmt.Errorf("range is required")
	}
	if strings.Contains(rng, "..") || strings.ContainsAny(rng, "\n\r\t") {
		return "", fmt.Errorf("range looks invalid")
	}
	if !a1RangeRe.MatchString(rng) {
		return "", fmt.Errorf("range looks invalid")
	}
	// Encourage explicit sheet names for safety.
	if !strings.Contains(rng, "!") {
		return "", fmt.Errorf("range must be in A1 notation with a sheet name, e.g. Sheet1!A1:D10")
	}
	return rng, nil
}

func require2DValues(params map[string]any, key string) ([][]any, error) {
	raw, ok := params[key]
	if !ok {
		return nil, fmt.Errorf("%s is required", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of rows", key)
	}
	out := make([][]any, 0, len(arr))
	for _, row := range arr {
		rowArr, ok := row.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of rows (each row is an array)", key)
		}
		// Clamp row width to avoid accidental huge payloads.
		if len(rowArr) > 200 {
			return nil, fmt.Errorf("row has too many columns (max 200)")
		}
		out = append(out, rowArr)
	}
	return out, nil
}
