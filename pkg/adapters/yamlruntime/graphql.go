package yamlruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// executeGraphQL executes a GraphQL action as defined in the YAML spec.
func executeGraphQL(ctx context.Context, client *http.Client, baseURL string, action yamldef.Action, params map[string]any, serviceID string) (*adapters.Result, error) {
	variables := map[string]any{}
	filter := map[string]any{}
	inputObj := map[string]any{}

	for name, paramDef := range action.Params {
		val, _ := resolveParamWithExpr(params, name, paramDef, nil)
		if val == nil {
			continue
		}

		switch {
		case paramDef.GraphQLVar:
			variables[name] = val
		case paramDef.FilterPath != "":
			setNestedValue(filter, paramDef.FilterPath, val)
		case paramDef.InputField != "":
			inputObj[paramDef.InputField] = val
		}
	}

	if len(filter) > 0 {
		variables["filter"] = filter
	}
	if len(inputObj) > 0 {
		variables["input"] = inputObj
	}

	// Build the GraphQL payload.
	payload := map[string]any{"query": action.Query}
	if len(variables) > 0 {
		payload["variables"] = variables
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling GraphQL payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("building GraphQL request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing GraphQL request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	// Check for GraphQL-level errors.
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("%s: graphql error: %s", serviceID, envelope.Errors[0].Message)
	}

	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing GraphQL response: %w", err)
	}

	data := extractData(raw, action.Response, nil)
	meta := extractMeta(raw, action.Response.Meta)
	summary := renderSummary(action.Response.Summary, data)

	return &adapters.Result{
		Summary: summary,
		Data:    data,
		Meta:    meta,
	}, nil
}

// setNestedValue creates nested map structure from a dot-delimited path and sets the value.
// e.g. setNestedValue(m, "team.id.eq", "abc") produces {"team": {"id": {"eq": "abc"}}}.
func setNestedValue(m map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := m
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = value
			return
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
}
